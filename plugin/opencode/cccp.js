// @cccp-plugin opencode
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { spawnSync } from "node:child_process"
import { createHash } from "node:crypto"

const AGENT = "opencode"
const CCCP = process.env.CCCP_BIN || "cccp"
const BACKFILL = process.env.CCCP_BACKFILL ?? ""
const BACKFILL_DELAY_MS = envInt("CCCP_BACKFILL_DELAY_MS", 2000)
const BACKFILL_BUDGET_MS = envInt("CCCP_BACKFILL_BUDGET_MS", 20000)
const BACKFILL_PAGE = 50
const BACKFILL_GAP_MS = 100
const BACKFILL_LOCK_STALE_MS = 10 * 60 * 1000
const SEEN_MAX = 8192

function envInt(name, fallback) {
  const n = Number(process.env[name])
  return Number.isFinite(n) && n >= 0 ? n : fallback
}

const TOOL_NAMES = {
  bash: "Bash",
  shell: "Bash",
  read: "Read",
  write: "Write",
  edit: "Edit",
  multiedit: "MultiEdit",
  patch: "Edit",
  apply_patch: "Edit",
  glob: "Glob",
  grep: "Grep",
  list: "LS",
  webfetch: "WebFetch",
  websearch: "WebSearch",
  todowrite: "TodoWrite",
  todo: "TodoWrite",
  todoread: "TodoRead",
  task: "Agent",
  subagent: "Agent",
  question: "AskUserQuestion",
  skill: "Skill",
}
const INPUT_ALIASES = { filePath: "file_path", oldString: "old_string", newString: "new_string" }
const PATH_TOOLS = new Set(["read", "write", "edit"])
const META_TEXT = /^\s*<[a-z][a-z0-9_-]*>/
const META_LINES = new Set(["session-meta", "ai-title"])
const TERMINAL = new Set(["session.execution.succeeded", "session.execution.failed", "session.execution.interrupted"])

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

const STATE_KEY = Symbol.for(`cccp.opencode:${ROOT}`)
const state = (globalThis[STATE_KEY] ??= {
  sessions: new Map(),
  seen: new Set(),
  exitHooked: false,
  missingLogged: false,
  closing: false,
  backfillStarted: false,
  backfillRun: null,
})
const sessions = state.sessions

function log(msg) {
  try {
    fs.mkdirSync(path.dirname(LOG), { recursive: true })
    fs.appendFileSync(LOG, `${new Date().toISOString()} ${msg}\n`)
  } catch {}
}

function iso(ms) {
  return new Date(typeof ms === "number" && ms > 0 ? ms : Date.now()).toISOString()
}

function aliasInput(tool, input) {
  if (!input || typeof input !== "object") return input ?? {}
  const out = { ...input }
  for (const [from, to] of Object.entries(INPUT_ALIASES)) {
    if (from in out && !(to in out)) out[to] = out[from]
  }
  if (PATH_TOOLS.has(tool) && typeof out.path === "string" && !("file_path" in out)) out.file_path = out.path
  return out
}

function firstSeen(id) {
  if (!id) return true
  if (state.seen.has(id)) return false
  state.seen.add(id)
  if (state.seen.size > SEEN_MAX) state.seen.delete(state.seen.values().next().value)
  return true
}

function spawnError(which, s, err) {
  if (err?.code === "ENOENT") {
    if (state.missingLogged) return
    state.missingLogged = true
    log(`${CCCP} not found on PATH; transcripts are written but not synced`)
    return
  }
  log(`hook-${which} ${s.id}: ${err?.message ?? err}`)
}

function readWritten(file) {
  const written = new Set()
  let last = null
  let model = null
  let lastTs = 0
  let data
  try {
    data = fs.readFileSync(file, "utf8")
  } catch {
    return { written, last, model, lastTs }
  }
  for (const line of data.split("\n")) {
    if (!line.trim()) continue
    try {
      const parsed = JSON.parse(line)
      if (parsed.uuid) {
        written.add(parsed.uuid)
        last = parsed.uuid
      }
      if (!META_LINES.has(parsed.type)) lastTs = Math.max(lastTs, Date.parse(parsed.timestamp) || 0)
      if (parsed.type === "model-change" && parsed.model) model = parsed.model
      else if (parsed.type === "assistant" && parsed.message?.model) model = parsed.message.model
    } catch {}
  }
  return { written, last, model, lastTs }
}

function newSession(id, { info, directory } = {}) {
  const file = path.join(TRANSCRIPTS, `${id}.jsonl`)
  const { written, last, model, lastTs } = readWritten(file)
  return {
    id,
    file,
    written,
    last,
    model,
    lastTs,
    info: info ?? null,
    directory,
    steps: new Map(),
    inbox: new Map(),
    hooked: false,
    loading: null,
    queue: [],
  }
}

function ensureSession(id, opts = {}) {
  let s = sessions.get(id)
  if (!s) {
    s = newSession(id, opts)
    sessions.set(id, s)
  }
  return s
}

function append(s, uuid, line) {
  if (s.written.has(uuid)) return
  if (line.type !== "session-meta" && line.type !== "ai-title") flushMeta(s, s.metaOpts)
  line.uuid = uuid
  line.parentUuid = s.last
  fs.mkdirSync(TRANSCRIPTS, { recursive: true })
  fs.appendFileSync(s.file, JSON.stringify(line) + "\n")
  s.written.add(uuid)
  s.last = uuid
}

function cwdOf(s) {
  return s.info?.directory ?? s.directory ?? process.cwd()
}

function base(s, type, ts) {
  return {
    parentUuid: null,
    isSidechain: false,
    userType: "external",
    cwd: cwdOf(s),
    sessionId: s.id,
    version: s.info?.version ?? "",
    agent: AGENT,
    type,
    uuid: "",
    timestamp: iso(ts),
  }
}

function gitInfo(dir) {
  const out = {}
  if (!dir) return out
  for (const [key, args] of [["gitBranch", ["rev-parse", "--abbrev-ref", "HEAD"]], ["gitCommit", ["rev-parse", "HEAD"]]]) {
    let r
    try {
      r = spawnSync("git", ["-C", dir, ...args], { encoding: "utf8", timeout: 2000 })
    } catch {
      return out
    }
    const v = (r.stdout ?? "").trim()
    if (r.status !== 0 || !v) return out
    if (v !== "HEAD") out[key] = v
  }
  return out
}

function userName() {
  if (process.env.USER) return process.env.USER
  try {
    return os.userInfo().username
  } catch {
    return ""
  }
}

function flushMeta(s, { git = true } = {}) {
  const info = s.info
  if (!info) return
  const uuid = `${s.id}-meta`
  if (!s.written.has(uuid)) {
    const line = { ...base(s, "session-meta", info.created), agentSource: info.parentID ? "subagent" : "" }
    if (info.parentID) line.parentSessionId = info.parentID
    const user = userName()
    if (user) line.user = user
    if (git) Object.assign(line, gitInfo(cwdOf(s)))
    append(s, uuid, line)
  }
  const title = typeof info.title === "string" ? info.title.trim() : ""
  if (title && !/^(New session|Child session) - /.test(title)) {
    const tuuid = `${s.id}-title-${createHash("sha1").update(title).digest("hex").slice(0, 12)}`
    if (!s.written.has(tuuid)) append(s, tuuid, { ...base(s, "ai-title", info.updated ?? info.created), aiTitle: title })
  }
}

function infoFromSession(row, version) {
  const info = { id: row.id, directory: row.location?.directory ?? row.directory, version: version ?? "", title: row.title }
  if (row.parentID) info.parentID = row.parentID
  if (row.time?.created) info.created = row.time.created
  if (row.time?.updated) info.updated = row.time.updated
  return info
}

function promptText(text, files) {
  const texts = []
  if (typeof text === "string" && text) texts.push(text)
  for (const f of Array.isArray(files) ? files : []) texts.push(`[file: ${f?.name ?? f?.source?.uri ?? ""}]`)
  return texts.join("\n\n")
}

function writeUser(s, uuid, ts, text, meta) {
  if (s.written.has(uuid) || !text) return
  const line = { ...base(s, "user", ts) }
  if (meta || META_TEXT.test(text)) line.isMeta = true
  line.message = { role: "user", content: text }
  append(s, uuid, line)
}

function modelOf(model) {
  if (!model) return ""
  if (typeof model === "string") return model
  return model.providerID && model.id ? `${model.providerID}/${model.id}` : (model.id ?? "")
}

function newStep(id, model, created) {
  return { id, model: modelOf(model), created, items: [], done: false, finish: null, tokens: null, cost: 0, error: null, completed: null, files: [], hash: "", ended: false }
}

function itemOf(step, key, kind) {
  let it = step.items.find((x) => x.key === key)
  if (!it) {
    it = { key, kind, final: false, text: "", start: null, end: null, name: "", callID: "", input: {}, result: "", isErr: false }
    step.items.push(it)
  }
  return it
}

function flushModel(s, step) {
  if (!step.model || step.model === s.model) return
  const uuid = `${step.id}-model`
  if (!s.written.has(uuid)) append(s, uuid, { ...base(s, "model-change", step.created), model: step.model, effort: "" })
  s.model = step.model
}

function assistantLine(s, step, ts, block, last) {
  const msg = {
    id: step.id,
    type: "message",
    role: "assistant",
    model: step.model,
    content: [block],
    stop_reason: last ? (step.finish ?? null) : null,
    stop_sequence: null,
  }
  if (last && step.tokens) {
    msg.usage = {
      input_tokens: step.tokens.input ?? 0,
      output_tokens: step.tokens.output ?? 0,
      cache_read_input_tokens: step.tokens.cache?.read ?? 0,
      cache_creation_input_tokens: step.tokens.cache?.write ?? 0,
    }
  }
  return { ...base(s, "assistant", ts), message: msg }
}

function itemUuid(step, it) {
  return `${step.id}-${it.key}`
}

function flushStep(s, step, force = false) {
  flushModel(s, step)
  const closing = step.done || force
  const items = step.items.filter((it) => !(it.kind === "text" && it.final && !it.text.trim()))
  let prevEnd = step.created
  for (let i = 0; i < items.length; i++) {
    const it = items[i]
    const ts = it.kind === "text" ? prevEnd : (it.start ?? prevEnd)
    if (it.end != null) prevEnd = it.end
    else if (it.kind !== "text" && it.start != null) prevEnd = it.start
    const uuid = itemUuid(step, it)
    if (s.written.has(uuid)) continue
    const last = i === items.length - 1
    if (!closing && (!it.final || last)) break
    switch (it.kind) {
      case "text":
        if (it.text.trim()) append(s, uuid, assistantLine(s, step, ts, { type: "text", text: it.text }, closing && last))
        break
      case "reasoning":
        append(s, uuid, assistantLine(s, step, ts, { type: "thinking", thinking: it.text ?? "", signature: "" }, closing && last))
        break
      case "tool": {
        append(s, uuid, assistantLine(s, step, ts, { type: "tool_use", id: it.callID, name: TOOL_NAMES[it.name] ?? it.name, input: aliasInput(it.name, it.input) }, false))
        const result = {
          ...base(s, "user", it.end ?? ts),
          message: { role: "user", content: [{ type: "tool_result", tool_use_id: it.callID, content: it.final ? it.result : "", is_error: it.isErr }] },
        }
        append(s, `${uuid}-result`, result)
        break
      }
    }
  }
  if (!closing) return
  if (step.error && !s.written.has(`${step.id}-error`)) {
    const e = step.error
    const text = `Error: ${e.type ?? e.name ?? "UnknownError"}${e.message ? `: ${e.message}` : ""}`
    append(s, `${step.id}-error`, assistantLine(s, step, step.completed ?? prevEnd, { type: "text", text }, true))
  }
  if (!step.ended) return
  const ts = step.completed ?? prevEnd
  const files = step.files.filter((f) => typeof f === "string")
  if (files.length && !s.written.has(`${step.id}-patch`)) {
    append(s, `${step.id}-patch`, { ...base(s, "system", ts), subtype: "patch", content: files.join("\n"), files, hash: step.hash ?? "" })
  }
  if (step.tokens && !s.written.has(`${step.id}-usage`)) {
    const t = step.tokens
    append(s, `${step.id}-usage`, {
      ...base(s, "usage", ts),
      model: step.model,
      usage: {
        input_tokens: t.input ?? 0,
        output_tokens: (t.output ?? 0) + (t.reasoning ?? 0),
        cache_read_input_tokens: t.cache?.read ?? 0,
        cache_creation_input_tokens: t.cache?.write ?? 0,
        reasoning_tokens: t.reasoning ?? 0,
      },
      costUSD: typeof step.cost === "number" ? step.cost : 0,
      stepReason: step.finish ?? "",
    })
  }
}

function flushAll(s, force) {
  for (const step of s.steps.values()) flushStep(s, step, force)
  if (force) s.steps.clear()
}

function contentText(content) {
  if (!Array.isArray(content)) return typeof content === "string" ? content : ""
  return content.map((c) => (c?.type === "text" ? (c.text ?? "") : `[${c?.type ?? "content"}]`)).join("\n")
}

function stepFromMessage(m) {
  const step = newStep(m.id, m.model, m.time?.created)
  const counts = { text: 0, reasoning: 0 }
  for (const c of m.content ?? []) {
    if (c?.type === "text" || c?.type === "reasoning") {
      const it = itemOf(step, `${c.type}-${counts[c.type]++}`, c.type)
      it.text = c.text ?? ""
      it.final = true
      if (c.type === "reasoning") {
        it.start = c.time?.created ?? null
        it.end = c.time?.completed ?? null
      }
    } else if (c?.type === "tool") {
      const it = itemOf(step, `tool-${c.id}`, "tool")
      it.callID = c.id
      it.name = c.name
      it.input = c.state?.input && typeof c.state.input === "object" ? c.state.input : {}
      it.start = c.time?.ran ?? c.time?.created ?? null
      it.end = c.time?.completed ?? null
      const st = c.state?.status
      it.final = st === "completed" || st === "error"
      it.isErr = st === "error"
      it.result = st === "error" ? String(c.state?.error?.message ?? "error") : contentText(c.state?.content)
    }
  }
  step.finish = m.finish ?? null
  step.tokens = m.tokens ?? null
  step.cost = m.cost ?? 0
  step.error = m.error ?? null
  step.completed = m.time?.completed ?? null
  step.files = m.snapshot?.files ?? []
  step.hash = m.snapshot?.end ?? ""
  step.done = m.time?.completed != null
  step.ended = step.done && step.tokens != null
  return step
}

function emitHistory(s, messages, all) {
  for (let i = 0; i < messages.length; i++) {
    const m = messages[i]
    if (!m?.id) continue
    switch (m.type) {
      case "user":
        if (m.time?.created <= s.lastTs) break
        writeUser(s, m.id, m.time?.created, promptText(m.text, m.files), false)
        break
      case "synthetic":
        if (m.time?.created <= s.lastTs) break
        writeUser(s, m.id, m.time?.created, m.text ?? "", true)
        break
      case "assistant": {
        if (s.steps.has(m.id)) return
        if (m.time?.completed != null && m.time.completed <= s.lastTs) break
        const step = stepFromMessage(m)
        if (!step.done && !all) {
          s.steps.set(m.id, step)
          flushStep(s, step)
          return
        }
        flushStep(s, step, true)
        break
      }
    }
  }
}

function hookPayload(s) {
  return JSON.stringify({ session_id: s.id, agent: AGENT, cwd: cwdOf(s) })
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
  if (s.hooked || !s.info || s.info.parentID) return
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

function shutdown() {
  state.closing = true
  let ended = false
  for (const s of sessions.values()) {
    try {
      drain(s)
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

function drain(s) {
  const queued = s.queue.splice(0)
  s.loading = null
  for (const e of queued) apply(s, e)
}

async function catchUp(s, ctx) {
  try {
    const row = await ctx.session.get({ sessionID: s.id })
    if (row?.id) {
      s.info = infoFromSession(row, ctx.app?.version)
      if (s.written.size) flushMeta(s)
    }
  } catch (err) {
    log(`session.get ${s.id}: ${err?.message ?? err}`)
  }
  if (!s.info) s.info = { id: s.id, directory: s.directory, version: ctx.app?.version ?? "" }
  try {
    const messages = await ctx.session.context({ sessionID: s.id })
    if (s.loading) emitHistory(s, Array.isArray(messages) ? messages : [], false)
  } catch (err) {
    log(`session.context ${s.id}: ${err?.message ?? err}`)
  }
}

function track(s, ctx) {
  if (s.info || s.loading) return
  if (!ctx?.session?.get) {
    s.info = { id: s.id, directory: s.directory, version: ctx?.app?.version ?? "" }
    hookStart(s)
    return
  }
  s.loading = catchUp(s, ctx)
    .catch((err) => log(`catch-up ${s.id}: ${err?.stack ?? err}`))
    .finally(() => {
      if (!sessions.has(s.id) || !s.loading) return
      try {
        hookStart(s)
        drain(s)
      } catch (err) {
        log(`drain ${s.id}: ${err?.stack ?? err}`)
      }
    })
}

function stepOf(s, d, created) {
  const id = d.assistantMessageID
  if (!id) return null
  let step = s.steps.get(id)
  if (!step) {
    step = newStep(id, null, created)
    s.steps.set(id, step)
  }
  return step
}

function apply(s, e) {
  const d = e.data ?? {}
  if (TERMINAL.has(e.type)) {
    flushAll(s, true)
    return
  }
  switch (e.type) {
    case "session.renamed":
      if (s.info) {
        s.info.title = d.title
        s.info.updated = e.created
        if (s.written.size) flushMeta(s)
      }
      return
    case "session.inbox.enqueued":
      s.inbox.set(d.inboxID, d.item)
      return
    case "session.inbox.cancelled":
      s.inbox.delete(d.inboxID)
      return
    case "session.inbox.delivered": {
      const item = s.inbox.get(d.inboxID)
      s.inbox.delete(d.inboxID)
      if (item?.type === "user") writeUser(s, d.inboxID, e.created, promptText(item.payload?.text, item.payload?.files), false)
      else if (item?.type === "synthetic") writeUser(s, d.inboxID, e.created, item.payload?.text ?? "", true)
      return
    }
    case "session.synthetic":
      writeUser(s, e.id.replace(/^evt_/, "msg_"), e.created, d.text ?? "", true)
      return
    case "session.step.started": {
      const step = stepOf(s, d, d.started ?? e.created)
      step.model = modelOf(d.model)
      step.created = d.started ?? e.created
      flushModel(s, step)
      return
    }
    case "session.text.started":
    case "session.reasoning.started": {
      const kind = e.type === "session.text.started" ? "text" : "reasoning"
      const step = stepOf(s, d, e.created)
      const it = itemOf(step, `${kind}-${d.ordinal}`, kind)
      if (kind === "reasoning") it.start = e.created
      return
    }
    case "session.text.ended":
    case "session.reasoning.ended": {
      const kind = e.type === "session.text.ended" ? "text" : "reasoning"
      const step = stepOf(s, d, e.created)
      const it = itemOf(step, `${kind}-${d.ordinal}`, kind)
      it.text = d.text ?? ""
      it.final = true
      if (kind === "reasoning") {
        it.start ??= e.created
        it.end = e.created
      }
      flushStep(s, step)
      return
    }
    case "session.tool.input.started": {
      const step = stepOf(s, d, e.created)
      const it = itemOf(step, `tool-${d.id}`, "tool")
      it.callID = d.id
      it.name = d.name ?? it.name
      return
    }
    case "session.tool.called": {
      const step = stepOf(s, d, e.created)
      const it = itemOf(step, `tool-${d.id}`, "tool")
      it.callID = d.id
      it.input = d.input && typeof d.input === "object" ? d.input : {}
      it.start = e.created
      return
    }
    case "session.tool.success":
    case "session.tool.failed": {
      const step = stepOf(s, d, e.created)
      const it = itemOf(step, `tool-${d.id}`, "tool")
      it.callID = d.id
      it.final = true
      it.end = e.created
      it.start ??= e.created
      it.isErr = e.type === "session.tool.failed"
      it.result = it.isErr ? String(d.error?.message ?? "error") : contentText(d.content)
      flushStep(s, step)
      return
    }
    case "session.step.ended":
    case "session.step.failed": {
      const step = stepOf(s, d, e.created)
      step.done = true
      step.ended = e.type === "session.step.ended" || d.tokens != null
      step.completed = e.created
      if (d.finish) step.finish = d.finish
      if (d.tokens) step.tokens = d.tokens
      if (typeof d.cost === "number") step.cost = d.cost
      if (Array.isArray(d.files)) step.files = d.files
      if (d.snapshot) step.hash = d.snapshot
      if (e.type === "session.step.failed") step.error = d.error ?? { type: "UnknownError" }
      flushStep(s, step)
      s.steps.delete(step.id)
      return
    }
  }
}

function handle(e, ctx) {
  if (!e?.type || !firstSeen(e.id)) return
  const d = e.data ?? {}
  const sid = d.sessionID
  if (!sid) return
  const directory = e.location?.directory ?? ctx?.location?.directory
  switch (e.type) {
    case "session.created": {
      const s = ensureSession(sid, { directory })
      s.info = { id: sid, directory: d.location?.directory ?? directory, version: d.version ?? ctx?.app?.version ?? "", title: d.title, created: e.created }
      if (d.parentID) s.info.parentID = d.parentID
      if (s.loading) s.loading = null
      if (s.written.size) flushMeta(s)
      hookStart(s)
      drain(s)
      return
    }
    case "session.deleted": {
      const s = sessions.get(sid)
      if (!s) return
      drain(s)
      flushAll(s, true)
      hookEnd(s)
      sessions.delete(sid)
      return
    }
  }
  if (!e.type.startsWith("session.")) return
  const s = ensureSession(sid, { directory })
  if (!s.directory) s.directory = directory
  track(s, ctx)
  if (s.loading) {
    s.queue.push(e)
    return
  }
  apply(s, e)
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

function anchorCursor(anchor) {
  if (!anchor?.id || typeof anchor.time !== "number") return undefined
  return Buffer.from(JSON.stringify({ anchor: { id: anchor.id, time: anchor.time, direction: "next" } })).toString("base64url")
}

async function listPage(ctx, anchor) {
  const query = { limit: BACKFILL_PAGE }
  const cursor = anchorCursor(anchor)
  if (cursor) query.cursor = cursor
  const res = await ctx.session.list(query)
  return Array.isArray(res?.data) ? res.data : Array.isArray(res) ? res : []
}

async function backfillSession(row, ctx, stats) {
  const s = newSession(row.id, { info: infoFromSession(row, ctx.app?.version) })
  const before = s.written.size
  s.metaOpts = { git: false }
  const messages = await ctx.session.context({ sessionID: row.id })
  emitHistory(s, Array.isArray(messages) ? messages : [], true)
  const added = s.written.size - before
  stats.sessions++
  if (added) {
    stats.lines += added
    if (before === 0) stats.files++
  }
}

function backfillStamp(run, complete) {
  const stamp = { updated: complete ? run.newest : run.updated, scope: "global", at: new Date().toISOString() }
  if (!complete) stamp.pending = { floor: run.floor ?? null, cursor: run.done ?? run.cursor ?? null, newest: run.newest }
  return stamp
}

function backfillSuspend() {
  const run = state.backfillRun
  state.backfillRun = null
  if (run) {
    try {
      writeJSON(STAMP, backfillStamp(run, false))
    } catch {}
  }
  unlockBackfill()
}

async function backfillHistory(ctx) {
  if (BACKFILL === "off") return
  if (!ctx?.session?.list || !ctx?.session?.context) return
  if (!lockBackfill()) {
    log("backfill: skipped, another process holds the lock")
    return
  }
  const t0 = Date.now()
  const stamp = (BACKFILL === "all" ? null : readJSON(STAMP)) ?? {}
  const pending = stamp.pending
  const validCursor = (c) => (c && typeof c === "object" && c.id && typeof c.time === "number" ? c : undefined)
  const run = {
    updated: stamp.updated ?? 0,
    floor: pending ? (pending.floor ?? undefined) : stamp.updated || undefined,
    cursor: pending ? validCursor(pending.cursor) : undefined,
    newest: pending ? (pending.newest ?? 0) : (stamp.updated ?? 0),
    done: undefined,
  }
  state.backfillRun = run
  const live = liveSessions()
  const stats = { sessions: 0, skipped: 0, files: 0, lines: 0, errors: 0 }
  let complete = false
  let cut = false
  try {
    while (!state.closing) {
      const rows = await listPage(ctx, run.cursor)
      for (const row of rows) {
        if (state.closing || Date.now() - t0 > BACKFILL_BUDGET_MS) {
          cut = true
          break
        }
        if (!row?.id) continue
        const updated = row.time?.updated ?? 0
        if (run.floor && updated < run.floor) {
          complete = true
          break
        }
        if (updated > run.newest) run.newest = updated
        if (live.has(row.id) || sessions.has(row.id)) {
          stats.skipped++
        } else {
          try {
            await backfillSession(row, ctx, stats)
          } catch (err) {
            stats.errors++
            log(`backfill ${row.id}: ${err?.message ?? err}`)
          }
          await pause(BACKFILL_GAP_MS)
        }
        run.done = { id: row.id, time: updated }
      }
      if (complete || cut || state.closing) break
      if (rows.length < BACKFILL_PAGE) {
        complete = true
        break
      }
      run.cursor = run.done
      await pause(BACKFILL_GAP_MS)
    }
    if (state.backfillRun === run) {
      writeJSON(STAMP, backfillStamp(run, complete))
      state.backfillRun = null
    }
    const ms = Date.now() - t0
    const tail = complete ? "complete" : `cut after ${ms} ms, resumes next launch`
    log(`backfill: scope=global sessions=${stats.sessions} skipped=${stats.skipped} files=${stats.files} lines=${stats.lines} errors=${stats.errors} ms=${ms} ${tail}`)
  } catch (err) {
    log(`backfill: ${err?.stack ?? err}`)
  } finally {
    if (state.backfillRun === run) state.backfillRun = null
    unlockBackfill()
  }
}

export function setup(ctx) {
  state.closing = false
  if (!state.exitHooked) {
    state.exitHooked = true
    process.once("exit", shutdown)
  }
  if (!state.backfillStarted && BACKFILL !== "off") {
    state.backfillStarted = true
    const timer = setTimeout(() => {
      backfillHistory(ctx).catch((err) => log(`backfill: ${err?.stack ?? err}`))
    }, BACKFILL_DELAY_MS)
    timer.unref?.()
  }
  const abort = new AbortController()
  const loop = (async () => {
    for await (const event of ctx.event.subscribe({ signal: abort.signal })) {
      try {
        handle(event, ctx)
      } catch (err) {
        log(`event ${event?.type}: ${err?.stack ?? err}`)
      }
    }
  })().catch((err) => {
    if (!abort.signal.aborted) log(`subscribe: ${err?.stack ?? err}`)
  })
  return async () => {
    abort.abort()
    await Promise.race([loop, pause(1000)])
    try {
      for (const s of sessions.values()) {
        drain(s)
        flushAll(s, false)
      }
    } catch (err) {
      log(`cleanup: ${err?.message ?? err}`)
    }
  }
}

export default { id: "cccp", setup }
