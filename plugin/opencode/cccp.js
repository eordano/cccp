// @cccp-plugin opencode
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { spawnSync } from "node:child_process"

const AGENT = "opencode"
const CCCP = process.env.CCCP_BIN || "cccp"
const BACKFILL = process.env.CCCP_BACKFILL ?? ""
const BACKFILL_DELAY_MS = envInt("CCCP_BACKFILL_DELAY_MS", 2000)
const BACKFILL_BUDGET_MS = envInt("CCCP_BACKFILL_BUDGET_MS", 20000)
const BACKFILL_PAGE = 50
const BACKFILL_GAP_MS = 100
const BACKFILL_LOCK_STALE_MS = 10 * 60 * 1000

function envInt(name, fallback) {
  const n = Number(process.env[name])
  return Number.isFinite(n) && n >= 0 ? n : fallback
}

const TOOL_NAMES = {
  bash: "Bash",
  read: "Read",
  write: "Write",
  edit: "Edit",
  multiedit: "MultiEdit",
  patch: "Edit",
  glob: "Glob",
  grep: "Grep",
  list: "LS",
  webfetch: "WebFetch",
  websearch: "WebSearch",
  todowrite: "TodoWrite",
  todoread: "TodoRead",
  task: "Agent",
  question: "AskUserQuestion",
  skill: "Skill",
}
const INPUT_ALIASES = { filePath: "file_path", oldString: "old_string", newString: "new_string" }
const SKIP_PARTS = new Set(["step-start", "snapshot", "agent", "retry", "compaction", "subtask"])
const META_PARTS = new Set(["step-finish", "patch"])
const META_TEXT = /^\s*<[a-z][a-z0-9_-]*>/

function dataRoot() {
  const xdg = process.env.XDG_DATA_HOME
  const base = xdg && xdg.length ? xdg : path.join(os.homedir(), ".local", "share")
  return path.join(base, AGENT)
}

const ROOT = dataRoot()
const TRANSCRIPTS = path.join(ROOT, "transcripts")
const LOG = path.join(ROOT, "cccp", "opencode-plugin.log")
const STAMP = path.join(ROOT, "cccp", "backfill.json")
const LOCK = path.join(ROOT, "cccp", "backfill.lock")

function log(msg) {
  try {
    fs.mkdirSync(path.dirname(LOG), { recursive: true })
    fs.appendFileSync(LOG, `${new Date().toISOString()} ${msg}\n`)
  } catch {}
}

function iso(ms) {
  return new Date(typeof ms === "number" && ms > 0 ? ms : Date.now()).toISOString()
}

function aliasInput(input) {
  if (!input || typeof input !== "object") return input ?? {}
  const out = { ...input }
  for (const [from, to] of Object.entries(INPUT_ALIASES)) {
    if (from in out && !(to in out)) out[to] = out[from]
  }
  return out
}

const sessions = new Map()
let exitHooked = false
let missingLogged = false

function spawnError(which, s, err) {
  if (err?.code === "ENOENT") {
    if (missingLogged) return
    missingLogged = true
    log(`${CCCP} not found on PATH; transcripts are written but not synced`)
    return
  }
  log(`hook-${which} ${s.id}: ${err?.message ?? err}`)
}

function readWritten(file) {
  const written = new Set()
  let last = null
  let model = null
  let data
  try {
    data = fs.readFileSync(file, "utf8")
  } catch {
    return { written, last, model }
  }
  for (const line of data.split("\n")) {
    if (!line.trim()) continue
    try {
      const parsed = JSON.parse(line)
      if (parsed.uuid) {
        written.add(parsed.uuid)
        last = parsed.uuid
      }
      if (parsed.type === "model-change" && parsed.model) model = parsed.model
      else if (parsed.type === "assistant" && parsed.message?.model) model = parsed.message.model
    } catch {}
  }
  return { written, last, model }
}

function newSession(id, { created = false, info, directory } = {}) {
  const file = path.join(TRANSCRIPTS, `${id}.jsonl`)
  const { written, last, model } = readWritten(file)
  return { id, file, written, last, model, info: info ?? null, directory, messages: new Map(), hooked: false, resumed: !created, backfilled: false }
}

function ensureSession(id, opts = {}) {
  let s = sessions.get(id)
  if (!s) {
    s = newSession(id, opts)
    sessions.set(id, s)
  }
  if (opts.info) s.info = opts.info
  return s
}

function ensureMessage(s, id) {
  let m = s.messages.get(id)
  if (!m) {
    m = { id, info: null, parts: new Map(), done: false, skipped: new Set() }
    s.messages.set(id, m)
  }
  return m
}

function append(s, uuid, line) {
  if (s.written.has(uuid)) return
  line.uuid = uuid
  line.parentUuid = s.last
  fs.mkdirSync(TRANSCRIPTS, { recursive: true })
  fs.appendFileSync(s.file, JSON.stringify(line) + "\n")
  s.written.add(uuid)
  s.last = uuid
}

function base(s, type, ts, cwd) {
  return {
    parentUuid: null,
    isSidechain: false,
    userType: "external",
    cwd: cwd ?? s.info?.directory ?? s.directory ?? process.cwd(),
    sessionId: s.id,
    version: s.info?.version ?? "",
    agent: AGENT,
    type,
    uuid: "",
    timestamp: iso(ts),
  }
}

function userText(m) {
  const texts = []
  let meta = false
  for (const p of m.parts.values()) {
    if (p.type === "text" && p.text) {
      texts.push(p.text)
      if (texts.length === 1) meta = p.synthetic === true || META_TEXT.test(p.text)
    } else if (p.type === "file") texts.push(`[file: ${p.filename ?? p.url ?? ""}]`)
  }
  return { text: texts.join("\n\n"), meta }
}

function flushUser(s, m) {
  if (s.written.has(m.id)) return
  const { text, meta } = userText(m)
  if (!text) return
  const info = m.info
  const line = { ...base(s, "user", info?.time?.created) }
  if (meta) line.isMeta = true
  line.message = { role: "user", content: text }
  append(s, m.id, line)
}

function modelOf(info) {
  return info?.providerID && info?.modelID ? `${info.providerID}/${info.modelID}` : info?.modelID ?? ""
}

function flushModel(s, m) {
  const model = modelOf(m.info)
  if (!model || model === s.model) return
  const uuid = `${m.id}-model`
  if (!s.written.has(uuid)) append(s, uuid, { ...base(s, "model-change", m.info.time?.created, m.info.path?.cwd), model, effort: "" })
  s.model = model
}

function flushStepParts(s, m) {
  const info = m.info
  const ts = info.time?.completed ?? info.time?.created
  for (const p of m.parts.values()) {
    if (!META_PARTS.has(p.type) || s.written.has(p.id)) continue
    if (p.type === "step-finish") {
      const t = p.tokens ?? {}
      append(s, p.id, {
        ...base(s, "usage", ts, info.path?.cwd),
        model: modelOf(info),
        usage: {
          input_tokens: t.input ?? 0,
          output_tokens: (t.output ?? 0) + (t.reasoning ?? 0),
          cache_read_input_tokens: t.cache?.read ?? 0,
          cache_creation_input_tokens: t.cache?.write ?? 0,
          reasoning_tokens: t.reasoning ?? 0,
        },
        costUSD: typeof p.cost === "number" ? p.cost : 0,
        stepReason: p.reason ?? "",
      })
    } else {
      const files = Array.isArray(p.files) ? p.files.filter((f) => typeof f === "string") : []
      if (!files.length) {
        m.skipped.add(p.id)
        continue
      }
      append(s, p.id, { ...base(s, "system", ts, info.path?.cwd), subtype: "patch", content: files.join("\n"), files, hash: p.hash ?? "" })
    }
  }
}

function partFinal(p) {
  switch (p.type) {
    case "text":
    case "reasoning":
      return p.time?.end != null
    case "tool":
      return p.state?.status === "completed" || p.state?.status === "error"
    default:
      return true
  }
}

function assistantLine(s, m, ts, block, last) {
  const info = m.info
  const msg = {
    id: m.id,
    type: "message",
    role: "assistant",
    model: modelOf(info),
    content: [block],
    stop_reason: last ? info?.finish ?? null : null,
    stop_sequence: null,
  }
  if (last && info?.tokens) {
    msg.usage = {
      input_tokens: info.tokens.input ?? 0,
      output_tokens: info.tokens.output ?? 0,
      cache_read_input_tokens: info.tokens.cache?.read ?? 0,
      cache_creation_input_tokens: info.tokens.cache?.write ?? 0,
    }
  }
  return { ...base(s, "assistant", ts, info?.path?.cwd), message: msg }
}

function flushAssistant(s, m, force) {
  if (!m.info) return
  flushModel(s, m)
  const parts = [...m.parts.values()].filter((p) => !(SKIP_PARTS.has(p.type) || META_PARTS.has(p.type) || (p.type === "text" && p.ignored)))
  for (let i = 0; i < parts.length; i++) {
    const p = parts[i]
    if (s.written.has(p.id) || m.skipped.has(p.id)) continue
    if (!partFinal(p) && !(force || m.done)) break
    const last = (m.done || force) && i === parts.length - 1
    switch (p.type) {
      case "text":
        if (p.text && p.text.trim()) {
          append(s, p.id, assistantLine(s, m, p.time?.start, { type: "text", text: p.text }, last))
        } else {
          m.skipped.add(p.id)
        }
        break
      case "reasoning":
        append(s, p.id, assistantLine(s, m, p.time?.start, { type: "thinking", thinking: p.text ?? "", signature: "" }, last))
        break
      case "tool": {
        const st = p.state ?? {}
        append(
          s,
          p.id,
          assistantLine(
            s,
            m,
            st.time?.start,
            { type: "tool_use", id: p.callID, name: TOOL_NAMES[p.tool] ?? p.tool, input: aliasInput(st.input) },
            false,
          ),
        )
        const isErr = st.status === "error"
        const content = isErr ? String(st.error ?? "error") : String(st.output ?? "")
        const result = {
          ...base(s, "user", st.time?.end ?? st.time?.start, m.info?.path?.cwd),
          message: { role: "user", content: [{ type: "tool_result", tool_use_id: p.callID, content, is_error: isErr }] },
        }
        if (st.title) result.toolUseResult = { title: st.title }
        append(s, `${p.id}-result`, result)
        break
      }
      default:
        m.skipped.add(p.id)
    }
  }
  if ((m.done || force) && m.info?.error && !s.written.has(`${m.id}-error`)) {
    const e = m.info.error
    const text = `Error: ${e.name ?? "UnknownError"}${e.data?.message ? `: ${e.data.message}` : ""}`
    append(s, `${m.id}-error`, assistantLine(s, m, m.info.time?.completed, { type: "text", text }, true))
  }
  if (m.done || force) flushStepParts(s, m)
}

function flushMessage(s, m, force = false) {
  if (!m.info) return
  if (m.info.role === "user") {
    if (force || m.done) flushUser(s, m)
    return
  }
  if (m.info.parentID) {
    const parent = s.messages.get(m.info.parentID)
    if (parent && parent.info?.role === "user") {
      parent.done = true
      flushUser(s, parent)
    }
  }
  flushAssistant(s, m, force)
}

function flushAll(s, force) {
  for (const m of s.messages.values()) flushMessage(s, m, force)
}

function hookPayload(s) {
  return JSON.stringify({ session_id: s.id, agent: AGENT, cwd: s.info?.directory ?? s.directory ?? process.cwd() })
}

function hookArgs(which) {
  return ["watch", `hook-${which}`, "--agent", AGENT, "--dir", ROOT]
}

function runHook(which, s) {
  const r = spawnSync(CCCP, hookArgs(which), { input: hookPayload(s), stdio: ["pipe", "ignore", "ignore"], env: process.env, timeout: 10000 })
  if (r.error) {
    spawnError(which, s, r.error)
    return false
  }
  return r.status === 0
}

function hookStart(s) {
  if (s.hooked) return
  s.hooked = true
  try {
    runHook("start", s)
  } catch (err) {
    spawnError("start", s, err)
  }
}

function hookEnd(s) {
  if (!s.hooked) return
  s.hooked = false
  try {
    runHook("end", s)
  } catch (err) {
    spawnError("end", s, err)
  }
}

function sleepSync(ms) {
  try {
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
  } catch {}
}

function awaitDaemonExit(deadlineMs) {
  const dir = path.join(ROOT, "cccp")
  try {
    if (JSON.parse(fs.readFileSync(path.join(dir, "sessions.json"), "utf8")).length !== 0) return
  } catch {
    return
  }
  const pid = path.join(dir, "secretary.pid")
  const until = Date.now() + deadlineMs
  while (fs.existsSync(pid) && Date.now() < until) sleepSync(50)
}

let closing = false

function shutdown() {
  closing = true
  let ended = false
  for (const s of sessions.values()) {
    try {
      flushAll(s, true)
    } catch (err) {
      log(`flush ${s.id}: ${err?.message ?? err}`)
    }
    if (s.hooked) ended = true
    hookEnd(s)
  }
  backfillSuspend()
  if (ended) awaitDaemonExit(3000)
}

async function loadMessages(s, client, all) {
  const res = await client.session.messages({ path: { id: s.id } })
  if (res?.error) throw new Error(res.error?.data?.message ?? JSON.stringify(res.error))
  const rows = res?.data ?? (Array.isArray(res) ? res : [])
  for (let i = 0; i < rows.length; i++) {
    const { info, parts } = rows[i]
    if (!info) continue
    const m = ensureMessage(s, info.id)
    m.info ??= info
    for (const p of parts ?? []) if (!m.parts.has(p.id)) m.parts.set(p.id, p)
    const force = all || (info.role === "user" ? i < rows.length - 1 : info.time?.completed != null)
    flushMessage(s, m, force)
  }
}

async function backfill(s, client) {
  if (s.backfilled || !client?.session?.messages) return
  s.backfilled = true
  try {
    await loadMessages(s, client, false)
  } catch (err) {
    log(`backfill ${s.id}: ${err?.message ?? err}`)
  }
}

function pause(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

function readJSON(file) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"))
  } catch {
    return null
  }
}

function writeJSON(file, value) {
  fs.mkdirSync(path.dirname(file), { recursive: true })
  fs.writeFileSync(`${file}.tmp`, JSON.stringify(value) + "\n")
  fs.renameSync(`${file}.tmp`, file)
}

function pidAlive(pid) {
  try {
    process.kill(pid, 0)
    return true
  } catch (err) {
    return err?.code === "EPERM"
  }
}

function lockBackfill() {
  fs.mkdirSync(path.dirname(LOCK), { recursive: true })
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      fs.writeFileSync(LOCK, String(process.pid), { flag: "wx" })
      return true
    } catch (err) {
      if (err?.code !== "EEXIST") return false
    }
    let stale = true
    try {
      const pid = Number(fs.readFileSync(LOCK, "utf8"))
      const age = Date.now() - fs.statSync(LOCK).mtimeMs
      stale = age > BACKFILL_LOCK_STALE_MS || !(pid > 0) || (pid !== process.pid && !pidAlive(pid))
    } catch {}
    if (!stale) return false
    try {
      fs.unlinkSync(LOCK)
    } catch {}
  }
  return false
}

function unlockBackfill() {
  try {
    if (Number(fs.readFileSync(LOCK, "utf8")) === process.pid) fs.unlinkSync(LOCK)
  } catch {}
}

function liveSessions() {
  const ids = new Set(sessions.keys())
  const listed = readJSON(path.join(ROOT, "cccp", "sessions.json"))
  if (Array.isArray(listed)) for (const id of listed) if (typeof id === "string") ids.add(id)
  return ids
}

function unwrapList(res) {
  if (res?.error) throw new Error(res.error?.data?.message ?? JSON.stringify(res.error))
  const rows = Array.isArray(res?.data) ? res.data : Array.isArray(res) ? res : []
  const header = res?.response?.headers?.get?.("x-next-cursor")
  const next = header != null && header !== "" ? Number(header) : null
  return { rows, next: Number.isFinite(next) ? next : null }
}

function listGlobal(client, { start, cursor }) {
  const query = { directory: "", archived: true, limit: BACKFILL_PAGE }
  if (start) query.start = start
  if (cursor) query.cursor = cursor
  return client.session.list({ url: "/experimental/session", query }).then(unwrapList)
}

function listProject(client, { start }) {
  const query = { scope: "project", limit: 1000 }
  if (start) query.start = start
  return client.session.list({ query }).then((res) => ({ rows: unwrapList(res).rows, next: null }))
}

async function backfillSession(row, client, stats) {
  const s = newSession(row.id, { info: row, directory: row.directory })
  const before = s.written.size
  await loadMessages(s, client, true)
  const added = s.written.size - before
  stats.sessions++
  if (added) {
    stats.lines += added
    if (before === 0) stats.files++
  }
}

let backfillStarted = false
let backfillRun = null

function backfillStamp(run, complete) {
  const stamp = { updated: complete ? run.newest : run.updated, scope: run.scope, at: new Date().toISOString() }
  if (!complete) stamp.pending = { floor: run.floor ?? null, cursor: run.done ?? run.cursor ?? null, newest: run.newest }
  return stamp
}

function backfillSuspend() {
  const run = backfillRun
  backfillRun = null
  if (run) {
    try {
      writeJSON(STAMP, backfillStamp(run, false))
    } catch {}
  }
  unlockBackfill()
}

async function backfillHistory(client) {
  if (backfillStarted || BACKFILL === "off") return
  backfillStarted = true
  if (!client?.session?.list || !client?.session?.messages) return
  if (!lockBackfill()) {
    log("backfill: skipped, another process holds the lock")
    return
  }
  const t0 = Date.now()
  const stamp = (BACKFILL === "all" ? null : readJSON(STAMP)) ?? {}
  const pending = stamp.pending
  const run = {
    updated: stamp.updated ?? 0,
    floor: pending ? (pending.floor ?? undefined) : stamp.updated || undefined,
    cursor: pending ? (pending.cursor ?? undefined) : undefined,
    newest: pending ? (pending.newest ?? 0) : (stamp.updated ?? 0),
    done: undefined,
    scope: "global",
  }
  backfillRun = run
  const live = liveSessions()
  const stats = { sessions: 0, skipped: 0, files: 0, lines: 0, errors: 0 }
  let complete = false
  let cut = false
  try {
    while (!closing) {
      let page
      try {
        page = await listGlobal(client, { start: run.floor, cursor: run.cursor })
      } catch (err) {
        if (run.cursor != null || run.scope !== "global") throw err
        log(`backfill: global listing failed (${err?.message ?? err}); falling back to this project's sessions`)
        run.scope = "project"
        page = await listProject(client, { start: run.floor })
      }
      for (const row of page.rows) {
        if (closing || Date.now() - t0 > BACKFILL_BUDGET_MS) {
          cut = true
          break
        }
        if (!row?.id) continue
        const updated = row.time?.updated ?? 0
        if (updated > run.newest) run.newest = updated
        if (live.has(row.id) || sessions.has(row.id)) {
          stats.skipped++
        } else {
          try {
            await backfillSession(row, client, stats)
          } catch (err) {
            stats.errors++
            log(`backfill ${row.id}: ${err?.message ?? err}`)
          }
          await pause(BACKFILL_GAP_MS)
        }
        run.done = updated
      }
      if (cut || closing) break
      if (run.scope !== "global" || page.next == null || page.rows.length === 0) {
        complete = true
        break
      }
      run.cursor = page.next
      await pause(BACKFILL_GAP_MS)
    }
    if (backfillRun === run) {
      writeJSON(STAMP, backfillStamp(run, complete))
      backfillRun = null
    }
    const ms = Date.now() - t0
    const tail = complete ? "complete" : `cut after ${ms} ms, resumes next launch`
    log(`backfill: scope=${run.scope} sessions=${stats.sessions} skipped=${stats.skipped} files=${stats.files} lines=${stats.lines} errors=${stats.errors} ms=${ms} ${tail}`)
  } catch (err) {
    log(`backfill: ${err?.stack ?? err}`)
  } finally {
    if (backfillRun === run) backfillRun = null
    unlockBackfill()
  }
}

function handle(event, client, directory) {
  const props = event?.properties ?? {}
  switch (event?.type) {
    case "session.created": {
      const info = props.info
      if (!info?.id) return
      const s = ensureSession(info.id, { created: true, info, directory })
      if (!info.parentID) hookStart(s)
      return
    }
    case "session.updated": {
      const info = props.info
      if (!info?.id) return
      const s = ensureSession(info.id, { info, directory })
      if (!info.parentID) hookStart(s)
      if (s.resumed) void backfill(s, client)
      return
    }
    case "message.updated": {
      const info = props.info
      if (!info?.sessionID || !info.id) return
      const s = ensureSession(info.sessionID, { directory })
      hookStart(s)
      if (s.resumed) void backfill(s, client)
      const m = ensureMessage(s, info.id)
      m.info = info
      if (info.role === "assistant" && info.time?.completed != null) m.done = true
      flushMessage(s, m)
      return
    }
    case "message.part.updated": {
      const part = props.part
      if (!part?.sessionID || !part.messageID || !part.id) return
      const s = ensureSession(part.sessionID, { directory })
      const m = ensureMessage(s, part.messageID)
      m.parts.set(part.id, part)
      if (m.info?.role === "assistant") flushMessage(s, m)
      return
    }
    case "session.idle": {
      const s = sessions.get(props.sessionID)
      if (!s) return
      for (const m of s.messages.values()) m.done = true
      flushAll(s, true)
      if (s.resumed) void backfill(s, client)
      return
    }
    case "session.deleted": {
      const id = props.info?.id
      const s = id && sessions.get(id)
      if (!s) return
      flushAll(s, true)
      hookEnd(s)
      sessions.delete(id)
      return
    }
  }
}

export const CccpPlugin = async ({ client, directory }) => {
  if (!exitHooked) {
    exitHooked = true
    process.once("exit", shutdown)
  }
  if (!backfillStarted && BACKFILL !== "off") {
    const timer = setTimeout(() => {
      backfillHistory(client).catch((err) => log(`backfill: ${err?.stack ?? err}`))
    }, BACKFILL_DELAY_MS)
    timer.unref?.()
  }
  return {
    event: async ({ event }) => {
      try {
        handle(event, client, directory)
      } catch (err) {
        log(`event ${event?.type}: ${err?.stack ?? err}`)
      }
    },
    dispose: async () => {
      try {
        shutdown()
      } catch (err) {
        log(`dispose: ${err?.message ?? err}`)
      }
    },
  }
}
