import { test } from "node:test"
import assert from "node:assert/strict"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"

const PLUGIN = new URL("../cccp.js", import.meta.url).href
const FIXTURE_WRITE = process.env.CCCP_FIXTURE_WRITE
let seq = 0
let evtSeq = 0
process.setMaxListeners(0)

function scratch() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "cccp-backfill-"))
}

async function load(root, env = {}) {
  for (const k of Object.keys(process.env)) if (k.startsWith("CCCP_")) delete process.env[k]
  process.env.XDG_DATA_HOME = root
  delete globalThis[Symbol.for(`cccp.opencode:${path.join(root, "opencode")}`)]
  process.env.CCCP_BIN = path.join(root, "no-such-cccp")
  process.env.CCCP_BACKFILL_DELAY_MS = "0"
  process.env.USER = "dev"
  Object.assign(process.env, env)
  const mod = await import(`${PLUGIN}?i=${++seq}`)
  assert.equal(mod.default.id, "cccp")
  return mod.default
}

const paths = (root) => ({
  transcripts: path.join(root, "opencode", "transcripts"),
  stamp: path.join(root, "opencode", "cccp", "backfill.json"),
  lock: path.join(root, "opencode", "cccp", "backfill.lock"),
  log: path.join(root, "opencode", "cccp", "opencode-plugin.log"),
  sessions: path.join(root, "opencode", "cccp", "sessions.json"),
})

function readJSON(file) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"))
  } catch {
    return null
  }
}

async function until(pred, ms = 5000) {
  const end = Date.now() + ms
  while (Date.now() < end) {
    const v = pred()
    if (v) return v
    await new Promise((r) => setTimeout(r, 15))
  }
  throw new Error("timed out")
}

const complete = (file) => () => {
  const s = readJSON(file)
  return s && !s.pending ? s : null
}

const META_TYPES = new Set(["session-meta", "ai-title"])

function lines(file) {
  return rawLines(file).filter((l) => !META_TYPES.has(l.type))
}

function rawLines(file) {
  return fs
    .readFileSync(file, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l))
}

class Channel {
  constructor() {
    this.queue = []
    this.waiting = null
    this.acks = []
  }
  push(event) {
    return new Promise((resolve) => {
      this.queue.push({ event, resolve })
      this.wake()
    })
  }
  wake() {
    if (this.waiting && this.queue.length) {
      const w = this.waiting
      this.waiting = null
      const { event, resolve } = this.queue.shift()
      this.acks.push(resolve)
      w({ done: false, value: event })
    }
  }
  iterable(signal) {
    const ch = this
    return {
      [Symbol.asyncIterator]() {
        return {
          next() {
            for (const ack of ch.acks.splice(0)) ack()
            if (signal?.aborted) return Promise.resolve({ done: true, value: undefined })
            return new Promise((resolve) => {
              ch.waiting = resolve
              signal?.addEventListener("abort", () => resolve({ done: true, value: undefined }), { once: true })
              ch.wake()
            })
          },
          return() {
            return Promise.resolve({ done: true, value: undefined })
          },
        }
      },
    }
  }
}

const T0 = 1788800000000

function session(id, { updated, directory = "/work/proj", turns = 1, parentID, modelID = "llm-default", extra = () => ({}) } = {}) {
  const info = { id, projectID: "p1", title: `title ${id}`, location: { directory }, time: { created: updated - 60000, updated } }
  if (parentID) info.parentID = parentID
  const messages = []
  for (let t = 0; t < turns; t++) {
    const base = updated - 50000 + t * 10000
    messages.push({ id: `msg_u_${id}_${t}`, type: "user", text: `prompt ${t} for ${id}`, files: [], time: { created: base } })
    messages.push({
      id: `msg_a_${id}_${t}`,
      type: "assistant",
      agent: "build",
      model: { id: modelID, providerID: "decent" },
      content: [
        { type: "reasoning", text: "thinking...", time: { created: base + 200, completed: base + 900 } },
        {
          type: "tool",
          id: `call_${id}_${t}`,
          name: "shell",
          executed: false,
          state: { status: "completed", input: { command: "echo hi", description: "say hi" }, content: [{ type: "text", text: "hi\n" }], metadata: {} },
          time: { created: base + 950, ran: base + 1000, completed: base + 1500 },
        },
        { type: "text", text: `answer ${t}` },
      ],
      finish: "stop",
      cost: 0.0012,
      tokens: { input: 100, output: 20, reasoning: 10, cache: { read: 5, write: 0 } },
      time: { created: base + 100, completed: base + 5000 },
      ...extra(id, t, base),
    })
  }
  return { info, messages }
}

function cursorOf(row) {
  return Buffer.from(JSON.stringify({ anchor: { id: row.id, time: row.time.updated, direction: "next" } })).toString("base64url")
}

function stubCtx(sessions, { messageDelay = 0, directory = "/work/proj" } = {}) {
  const calls = { list: [], context: [], get: [] }
  const chan = new Channel()
  const desc = (rows) => [...rows].sort((a, b) => b.time.updated - a.time.updated || (a.id < b.id ? 1 : -1))
  const ctx = {
    app: { name: "cli", version: "2.1.4", channel: "stable" },
    location: { directory, project: { id: "p1", directory, canonical: directory } },
    options: {},
    event: { subscribe: ({ signal } = {}) => chan.iterable(signal) },
    session: {
      async list(query = {}) {
        calls.list.push(query)
        let rows = desc(sessions.map((s) => s.info))
        if (query.cursor) {
          const { anchor } = JSON.parse(Buffer.from(query.cursor, "base64url").toString("utf8"))
          rows = rows.filter((r) => r.time.updated < anchor.time || (r.time.updated === anchor.time && r.id < anchor.id))
        }
        const data = rows.slice(0, query.limit ?? 50)
        return { data, cursor: data.length ? { next: cursorOf(data.at(-1)) } : {} }
      },
      async get({ sessionID }) {
        calls.get.push(sessionID)
        const s = sessions.find((x) => x.info.id === sessionID)
        if (!s) throw new Error("Session not found")
        return s.info
      },
      async context({ sessionID }) {
        calls.context.push(sessionID)
        if (messageDelay) await new Promise((r) => setTimeout(r, messageDelay))
        const s = sessions.find((x) => x.info.id === sessionID)
        return s ? s.messages : []
      },
    },
  }
  const ev = (type, data, created, extra = {}) =>
    chan.push({ id: `evt_${String(++evtSeq).padStart(8, "0")}`, created, type, data, location: { directory }, ...extra })
  return { ctx, calls, ev }
}

async function replay(ev, s, { created = true } = {}) {
  const sid = s.info.id
  if (created) {
    const data = { sessionID: sid, location: s.info.location, version: "2.1.4" }
    if (s.info.parentID) data.parentID = s.info.parentID
    await ev("session.created", data, s.info.time.created)
    if (s.info.title) await ev("session.renamed", { sessionID: sid, title: s.info.title }, s.info.time.created + 1)
  }
  for (const m of s.messages) {
    if (m.type === "user") {
      await ev("session.inbox.enqueued", { sessionID: sid, inboxID: m.id, item: { type: "user", payload: { text: m.text, files: m.files }, delivery: "steer" } }, m.time.created - 1)
      await ev("session.inbox.delivered", { sessionID: sid, inboxID: m.id }, m.time.created)
      continue
    }
    if (m.type === "synthetic") {
      await ev("session.inbox.enqueued", { sessionID: sid, inboxID: m.id, item: { type: "synthetic", payload: { text: m.text }, delivery: "steer" } }, m.time.created - 1)
      await ev("session.inbox.delivered", { sessionID: sid, inboxID: m.id }, m.time.created)
      continue
    }
    if (m.type !== "assistant") continue
    const a = { sessionID: sid, assistantMessageID: m.id }
    await ev("session.step.started", { ...a, agent: m.agent, model: m.model, started: m.time.created }, m.time.created + 1)
    const counts = { text: 0, reasoning: 0 }
    let t = m.time.created + 2
    for (const c of m.content) {
      if (c.type === "text" || c.type === "reasoning") {
        const ordinal = counts[c.type]++
        await ev(`session.${c.type}.started`, { ...a, ordinal }, c.time?.created ?? t)
        await ev(`session.${c.type}.delta`, { ...a, ordinal, delta: c.text }, c.time?.created ?? t)
        await ev(`session.${c.type}.ended`, { ...a, ordinal, text: c.text }, c.time?.completed ?? t + 1)
        t = (c.time?.completed ?? t + 1) + 1
      } else if (c.type === "tool") {
        await ev("session.tool.input.started", { ...a, id: c.id, name: c.name }, c.time.created)
        await ev("session.tool.called", { ...a, id: c.id, input: c.state.input, executed: false }, c.time.ran)
        if (c.state.status === "error") await ev("session.tool.failed", { ...a, id: c.id, error: c.state.error, executed: false }, c.time.completed)
        else await ev("session.tool.success", { ...a, id: c.id, content: c.state.content, metadata: c.state.metadata, executed: false }, c.time.completed)
        t = c.time.completed + 1
      }
    }
    const ended = { ...a, finish: m.finish, cost: m.cost, tokens: m.tokens }
    if (m.snapshot?.end) ended.snapshot = m.snapshot.end
    if (m.snapshot?.files) ended.files = m.snapshot.files
    await ev("session.step.ended", ended, m.time.completed)
  }
  await ev("session.execution.succeeded", { sessionID: sid }, s.messages.at(-1)?.time?.completed ?? T0)
}

test("full pass writes every session, pages the listing, preserves timestamps and cwd", async () => {
  const root = scratch()
  const p = paths(root)
  const all = []
  for (let i = 0; i < 55; i++) all.push(session(`ses_${String(i).padStart(3, "0")}`, { updated: T0 + i * 1000, directory: i % 2 ? "/work/a" : "/work/b" }))
  all.push(session("ses_rich", { updated: T0 + 100000, turns: 2, directory: "/work/rich" }))
  const { ctx, calls } = stubCtx(all, { directory: "/work/a" })
  const plugin = await load(root)
  await plugin.setup(ctx)
  const stamp = await until(complete(p.stamp), 20000)

  assert.equal(stamp.updated, T0 + 100000)
  assert.equal(stamp.scope, "global")
  assert.equal(fs.readdirSync(p.transcripts).length, 56)
  assert.equal(calls.list.length, 2)
  assert.equal(calls.list[0].limit, 50)
  assert.equal(calls.list[0].cursor, undefined)
  assert.equal(calls.list[1].cursor, cursorOf(all[6].info))
  assert.equal(calls.context.length, 56)
  assert.ok(!fs.existsSync(p.lock))

  const rich = lines(path.join(p.transcripts, "ses_rich.jsonl"))
  assert.deepEqual(
    rich.map((l) => l.type),
    ["user", "model-change", "assistant", "assistant", "user", "assistant", "usage", "user", "assistant", "assistant", "user", "assistant", "usage"],
  )
  const [u, mc, think, tool, result, text, usage] = rich
  const base = T0 + 100000 - 50000
  assert.equal(u.uuid, "msg_u_ses_rich_0")
  const [meta, title] = rawLines(path.join(p.transcripts, "ses_rich.jsonl"))
  assert.deepEqual([meta.type, meta.uuid, meta.parentUuid, meta.user, meta.agentSource, meta.cwd, meta.version, "gitCommit" in meta],
    ["session-meta", "ses_rich-meta", null, "dev", "", "/work/rich", "2.1.4", false])
  assert.deepEqual([title.type, title.aiTitle, title.parentUuid], ["ai-title", "title ses_rich", "ses_rich-meta"])
  assert.equal(u.parentUuid, title.uuid)
  assert.equal(u.message.content, "prompt 0 for ses_rich")
  assert.equal(u.timestamp, new Date(base).toISOString())
  assert.equal(u.cwd, "/work/rich")
  assert.equal(u.sessionId, "ses_rich")
  assert.equal(u.agent, "opencode")
  assert.equal(mc.uuid, "msg_a_ses_rich_0-model")
  assert.equal(mc.model, "decent/llm-default")
  assert.equal(mc.effort, "")
  assert.equal(mc.timestamp, new Date(base + 100).toISOString())
  assert.equal(think.uuid, "msg_a_ses_rich_0-reasoning-0")
  assert.equal(think.parentUuid, mc.uuid)
  assert.equal(think.message.content[0].type, "thinking")
  assert.equal(think.timestamp, new Date(base + 200).toISOString())
  assert.equal(tool.uuid, "msg_a_ses_rich_0-tool-call_ses_rich_0")
  assert.equal(tool.timestamp, new Date(base + 1000).toISOString())
  assert.equal(tool.message.content[0].type, "tool_use")
  assert.equal(tool.message.content[0].name, "Bash")
  assert.equal(tool.message.content[0].id, "call_ses_rich_0")
  assert.equal(result.uuid, "msg_a_ses_rich_0-tool-call_ses_rich_0-result")
  assert.equal(result.timestamp, new Date(base + 1500).toISOString())
  assert.equal(result.message.content[0].tool_use_id, "call_ses_rich_0")
  assert.equal(result.message.content[0].content, "hi\n")
  assert.equal(text.uuid, "msg_a_ses_rich_0-text-0")
  assert.equal(text.timestamp, new Date(base + 1500).toISOString())
  assert.equal(text.message.content[0].text, "answer 0")
  assert.equal(text.message.stop_reason, "stop")
  assert.equal(text.message.usage.input_tokens, 100)
  assert.equal(text.message.model, "decent/llm-default")
  assert.equal(usage.uuid, "msg_a_ses_rich_0-usage")
  assert.equal(usage.model, "decent/llm-default")
  assert.equal(usage.timestamp, new Date(base + 5000).toISOString())
  assert.deepEqual(usage.usage, { input_tokens: 100, output_tokens: 30, cache_read_input_tokens: 5, cache_creation_input_tokens: 0, reasoning_tokens: 10 })
  assert.equal(usage.costUSD, 0.0012)
  assert.equal(usage.stepReason, "stop")
  assert.equal(usage.message, undefined)
  for (let i = 1; i < rich.length; i++) assert.equal(rich[i].parentUuid, rich[i - 1].uuid)
  assert.equal(new Set(rich.map((l) => l.uuid)).size, rich.length)
})

test("backfilled bytes equal the live event path for the same messages", async () => {
  const s = session("ses_same", { updated: T0 + 5000, turns: 2, directory: "/work/same" })

  const rootA = scratch()
  const pa = paths(rootA)
  await (await load(rootA)).setup(stubCtx([s], { directory: "/work/same" }).ctx)
  await until(complete(pa.stamp))

  const rootB = scratch()
  const pb = paths(rootB)
  const live = stubCtx([], { directory: "/work/same" })
  await (await load(rootB, { CCCP_BACKFILL: "off" })).setup(live.ctx)
  await replay(live.ev, s)

  const a = lines(path.join(pa.transcripts, "ses_same.jsonl"))
  const b = lines(path.join(pb.transcripts, "ses_same.jsonl"))
  assert.equal(a.length, 13)
  assert.equal(a.at(-2).message.stop_reason, "stop")
  assert.equal(b.at(-2).message.usage.output_tokens, 20)
  assert.equal(a.map((l) => JSON.stringify(l)).join("\n"), b.map((l) => JSON.stringify(l)).join("\n"))
  assert.ok(!fs.existsSync(pb.stamp))
  assert.equal(live.calls.get.length, 0)
})

test("the last line of a step is held until the step ends, so it carries stop_reason and usage", async () => {
  const root = scratch()
  const p = paths(root)
  const { ctx, ev } = stubCtx([])
  await (await load(root, { CCCP_BACKFILL: "off" })).setup(ctx)
  const a = { sessionID: "ses_hold", assistantMessageID: "msg_h1" }
  await ev("session.created", { sessionID: "ses_hold", location: { directory: "/work/proj" }, version: "2.1.4" }, T0)
  await ev("session.inbox.enqueued", { sessionID: "ses_hold", inboxID: "msg_hu", item: { type: "user", payload: { text: "hi", files: [{ name: "a.png", mime: "image/png", data: "", source: { type: "inline" } }] }, delivery: "steer" } }, T0 + 1)
  await ev("session.inbox.delivered", { sessionID: "ses_hold", inboxID: "msg_hu" }, T0 + 2)
  await ev("session.step.started", { ...a, agent: "build", model: { id: "m", providerID: "p" }, started: T0 + 3 }, T0 + 3)
  await ev("session.text.started", { ...a, ordinal: 0 }, T0 + 4)
  await ev("session.text.ended", { ...a, ordinal: 0, text: "first" }, T0 + 5)
  const file = path.join(p.transcripts, "ses_hold.jsonl")
  assert.deepEqual(lines(file).map((l) => l.type), ["user", "model-change"])
  assert.equal(lines(file)[0].message.content, "hi\n\n[file: a.png]")
  await ev("session.text.started", { ...a, ordinal: 1 }, T0 + 6)
  await ev("session.text.ended", { ...a, ordinal: 1, text: "second" }, T0 + 7)
  assert.deepEqual(lines(file).map((l) => l.uuid), ["msg_hu", "msg_h1-model", "msg_h1-text-0"])
  await ev("session.step.ended", { ...a, finish: "stop", cost: 0.5, tokens: { input: 1, output: 2, reasoning: 0, cache: { read: 0, write: 0 } } }, T0 + 8)
  const ls = lines(file)
  assert.deepEqual(ls.map((l) => l.uuid), ["msg_hu", "msg_h1-model", "msg_h1-text-0", "msg_h1-text-1", "msg_h1-usage"])
  assert.equal(ls[2].message.stop_reason, null)
  assert.equal(ls[3].message.stop_reason, "stop")
  assert.equal(ls[3].message.usage.output_tokens, 2)
})

test("an interrupted step is flushed whole when the execution ends", async () => {
  const root = scratch()
  const p = paths(root)
  const { ctx, ev } = stubCtx([])
  await (await load(root, { CCCP_BACKFILL: "off" })).setup(ctx)
  const a = { sessionID: "ses_int", assistantMessageID: "msg_i1" }
  await ev("session.created", { sessionID: "ses_int", location: { directory: "/work/proj" }, version: "2.1.4" }, T0)
  await ev("session.step.started", { ...a, agent: "build", model: { id: "m", providerID: "p" }, started: T0 + 3 }, T0 + 3)
  await ev("session.text.started", { ...a, ordinal: 0 }, T0 + 4)
  await ev("session.text.ended", { ...a, ordinal: 0, text: "partial" }, T0 + 5)
  await ev("session.tool.input.started", { ...a, id: "call_x", name: "shell" }, T0 + 6)
  await ev("session.tool.called", { ...a, id: "call_x", input: { command: "sleep 9" }, executed: false }, T0 + 7)
  await ev("session.step.failed", { ...a, error: { type: "aborted", message: "interrupted by user" } }, T0 + 8)
  await ev("session.execution.interrupted", { sessionID: "ses_int", reason: "user" }, T0 + 9)
  const ls = lines(path.join(p.transcripts, "ses_int.jsonl"))
  assert.deepEqual(ls.map((l) => l.uuid), ["msg_i1-model", "msg_i1-text-0", "msg_i1-tool-call_x", "msg_i1-tool-call_x-result", "msg_i1-error"])
  assert.equal(ls[3].message.content[0].content, "")
  assert.equal(ls[4].message.content[0].text, "Error: aborted: interrupted by user")
})

test("a session first seen mid-turn is caught up from session.get and session.context", async () => {
  const root = scratch()
  const p = paths(root)
  const s = session("ses_late", { updated: T0 + 9000, turns: 2, directory: "/work/late" })
  const { ctx, calls, ev } = stubCtx([s])
  await (await load(root, { CCCP_BACKFILL: "off" })).setup(ctx)
  const tail = { ...s, messages: s.messages.slice(3) }
  await replay(ev, tail, { created: false })
  await until(() => fs.existsSync(path.join(p.transcripts, "ses_late.jsonl")) && lines(path.join(p.transcripts, "ses_late.jsonl")).length === 13)
  assert.deepEqual(calls.get, ["ses_late"])
  assert.deepEqual(calls.context, ["ses_late"])
  const [meta] = rawLines(path.join(p.transcripts, "ses_late.jsonl"))
  assert.deepEqual([meta.type, meta.cwd, meta.version], ["session-meta", "/work/late", "2.1.4"])
  const ls = lines(path.join(p.transcripts, "ses_late.jsonl"))
  assert.equal(ls[0].uuid, "msg_u_ses_late_0")
  assert.equal(ls.at(-1).uuid, "msg_a_ses_late_1-usage")
  assert.equal(new Set(ls.map((l) => l.uuid)).size, 13)
})

test("resuming a session recorded by the 1.x plugin appends only the new turn", async () => {
  const root = scratch()
  const p = paths(root)
  const s = session("ses_old1x", { updated: T0 + 9000, turns: 2, directory: "/work/old" })
  fs.mkdirSync(p.transcripts, { recursive: true })
  const file = path.join(p.transcripts, "ses_old1x.jsonl")
  const old = [
    { type: "session-meta", uuid: "ses_old1x-meta", timestamp: new Date(T0).toISOString() },
    { type: "user", uuid: "msg_u_ses_old1x_0", timestamp: new Date(T0 + 9000 - 50000).toISOString(), message: { role: "user", content: "prompt 0" } },
    { type: "assistant", uuid: "prt_text_0", timestamp: new Date(T0 + 9000 - 48000).toISOString(), message: { model: "decent/llm-default", content: [] } },
    { type: "usage", uuid: "prt_sf_0", timestamp: new Date(T0 + 9000 - 45000).toISOString() },
  ]
  fs.writeFileSync(file, old.map((l) => JSON.stringify(l)).join("\n") + "\n")
  const { ctx, calls, ev } = stubCtx([s])
  await (await load(root, { CCCP_BACKFILL: "off" })).setup(ctx)
  await replay(ev, { info: s.info, messages: s.messages.slice(2) }, { created: false })
  await until(() => lines(file).some((l) => l.uuid === "msg_a_ses_old1x_1-usage"))
  assert.deepEqual(calls.context, ["ses_old1x"])
  assert.deepEqual(
    lines(file).map((l) => l.uuid),
    ["msg_u_ses_old1x_0", "prt_text_0", "prt_sf_0", "msg_u_ses_old1x_1", "msg_a_ses_old1x_1-reasoning-0", "msg_a_ses_old1x_1-tool-call_ses_old1x_1", "msg_a_ses_old1x_1-tool-call_ses_old1x_1-result", "msg_a_ses_old1x_1-text-0", "msg_a_ses_old1x_1-usage"],
  )
})

test("events from several plugin instances are handled once; child sessions get no hook", async () => {
  const root = scratch()
  const p = paths(root)
  const hooks = path.join(root, "hooks.log")
  const bin = path.join(root, "cccp")
  fs.writeFileSync(bin, `#!/bin/sh\nprintf '%s %s\\n' "$2" "$(cat)" >> '${hooks}'\n`, { mode: 0o755 })
  const plugin = await load(root, { CCCP_BACKFILL: "off", CCCP_BIN: bin })
  const one = stubCtx([])
  const two = stubCtx([])
  await plugin.setup(one.ctx)
  await plugin.setup(two.ctx)
  const s = session("ses_twice", { updated: T0 + 1000 })
  const child = session("ses_child", { updated: T0 + 1000, parentID: "ses_twice" })
  const both = async (type, data, created) => {
    const id = `evt_dup_${++evtSeq}`
    const e = { id, created, type, data, location: { directory: "/work/proj" } }
    await one.ev.call(null, type, data, created, { id })
    await two.ev.call(null, type, data, created, { id })
    return e
  }
  await replay(both, s)
  await replay(both, child)
  assert.equal(lines(path.join(p.transcripts, "ses_twice.jsonl")).length, 7)
  const [cmeta] = rawLines(path.join(p.transcripts, "ses_child.jsonl"))
  assert.deepEqual([cmeta.agentSource, cmeta.parentSessionId], ["subagent", "ses_twice"])
  const calls = fs.readFileSync(hooks, "utf8").trim().split("\n")
  assert.equal(calls.length, 1)
  assert.match(calls[0], /^hook-start \{"session_id":"ses_twice","agent":"opencode","cwd":"\/work\/proj"\}$/)
})

test("second launch is incremental and writes nothing for unchanged sessions", async () => {
  const root = scratch()
  const p = paths(root)
  const all = [session("ses_a", { updated: T0 + 1000 }), session("ses_b", { updated: T0 + 2000 }), session("ses_c", { updated: T0 + 3000 })]

  await (await load(root)).setup(stubCtx(all).ctx)
  const stamp1 = await until(complete(p.stamp))
  const snapshot = Object.fromEntries(fs.readdirSync(p.transcripts).map((f) => [f, fs.readFileSync(path.join(p.transcripts, f), "utf8")]))

  const second = stubCtx(all)
  await (await load(root)).setup(second.ctx)
  const stamp2 = await until(() => {
    const s = complete(p.stamp)()
    return s && s.at !== stamp1.at ? s : null
  })
  assert.deepEqual(second.calls.context, ["ses_c"])
  assert.equal(stamp2.updated, T0 + 3000)
  for (const [f, body] of Object.entries(snapshot)) assert.equal(fs.readFileSync(path.join(p.transcripts, f), "utf8"), body)

  all.push(session("ses_d", { updated: T0 + 9000 }))
  all[0].info.time.updated = T0 + 8000
  all[0].messages.push(...session("ses_a", { updated: T0 + 8000, turns: 2 }).messages.slice(2))
  const third = stubCtx(all)
  await (await load(root)).setup(third.ctx)
  const stamp3 = await until(() => {
    const s = complete(p.stamp)()
    return s && s.at !== stamp2.at ? s : null
  })
  assert.deepEqual(third.calls.context.sort(), ["ses_a", "ses_c", "ses_d"])
  assert.equal(stamp3.updated, T0 + 9000)
  assert.equal(lines(path.join(p.transcripts, "ses_a.jsonl")).length, 13)
  assert.equal(new Set(lines(path.join(p.transcripts, "ses_a.jsonl")).map((l) => l.uuid)).size, 13)
  assert.ok(fs.existsSync(path.join(p.transcripts, "ses_d.jsonl")))
})

test("live sessions are skipped: this process's and the daemon's sessions.json", async () => {
  const root = scratch()
  const p = paths(root)
  fs.mkdirSync(path.dirname(p.sessions), { recursive: true })
  fs.writeFileSync(p.sessions, JSON.stringify(["ses_other"]))
  const all = [session("ses_other", { updated: T0 + 1000 }), session("ses_mine", { updated: T0 + 2000 }), session("ses_old", { updated: T0 + 3000 })]
  const { ctx, calls, ev } = stubCtx(all)
  await (await load(root, { CCCP_BACKFILL_DELAY_MS: "200" })).setup(ctx)
  await ev("session.created", { sessionID: "ses_mine", location: { directory: "/work/proj" }, version: "2.1.4" }, T0)
  const stamp = await until(complete(p.stamp))
  assert.deepEqual(calls.context, ["ses_old"])
  assert.deepEqual(fs.readdirSync(p.transcripts), ["ses_old.jsonl"])
  assert.equal(stamp.updated, T0 + 3000)
})

test("time budget cuts the pass, the stamp records the cursor, the next launch resumes without duplicates", async () => {
  const root = scratch()
  const p = paths(root)
  const all = []
  for (let i = 0; i < 10; i++) all.push(session(`ses_${i}`, { updated: T0 + i * 1000 }))

  const first = stubCtx(all, { messageDelay: 40 })
  await (await load(root, { CCCP_BACKFILL_BUDGET_MS: "100" })).setup(first.ctx)
  const stamp1 = await until(() => {
    const s = readJSON(p.stamp)
    return s?.pending ? s : null
  })
  const done1 = fs.readdirSync(p.transcripts)
  assert.ok(done1.length >= 1 && done1.length < 10, `wrote ${done1.length}`)
  assert.equal(stamp1.updated, 0)
  assert.equal(stamp1.pending.floor, null)
  const lastDone = 10 - done1.length
  assert.deepEqual(stamp1.pending.cursor, { id: `ses_${lastDone}`, time: T0 + lastDone * 1000 })
  assert.equal(stamp1.pending.newest, T0 + 9000)
  assert.ok(!fs.existsSync(p.lock))

  const second = stubCtx(all)
  await (await load(root)).setup(second.ctx)
  const stamp2 = await until(complete(p.stamp))
  assert.equal(second.calls.list[0].cursor, cursorOf(all[lastDone].info))
  assert.equal(second.calls.context.length, 10 - done1.length)
  assert.equal(stamp2.updated, T0 + 9000)
  assert.equal(fs.readdirSync(p.transcripts).length, 10)
  for (const f of fs.readdirSync(p.transcripts)) {
    const ls = lines(path.join(p.transcripts, f))
    assert.equal(ls.length, 7)
    assert.equal(new Set(ls.map((l) => l.uuid)).size, 7)
  }
})

test("CCCP_BACKFILL=off disables the pass", async () => {
  const root = scratch()
  const p = paths(root)
  const { ctx, calls } = stubCtx([session("ses_x", { updated: T0 })])
  await (await load(root, { CCCP_BACKFILL: "off" })).setup(ctx)
  await new Promise((r) => setTimeout(r, 150))
  assert.equal(calls.list.length, 0)
  assert.ok(!fs.existsSync(p.stamp))
  assert.ok(!fs.existsSync(p.transcripts))
})

test("CCCP_BACKFILL=all ignores the stamp and re-walks everything", async () => {
  const root = scratch()
  const p = paths(root)
  const all = [session("ses_a", { updated: T0 + 1000 }), session("ses_b", { updated: T0 + 2000 })]
  await (await load(root)).setup(stubCtx(all).ctx)
  const stamp1 = await until(complete(p.stamp))
  const { ctx, calls } = stubCtx(all)
  await (await load(root, { CCCP_BACKFILL: "all" })).setup(ctx)
  await until(() => {
    const s = complete(p.stamp)()
    return s && s.at !== stamp1.at ? s : null
  })
  assert.equal(calls.list[0].cursor, undefined)
  assert.deepEqual(calls.context.sort(), ["ses_a", "ses_b"])
  for (const f of fs.readdirSync(p.transcripts)) assert.equal(lines(path.join(p.transcripts, f)).length, 7)
})

test("a live lock skips the pass, a stale lock is reclaimed", async () => {
  const root = scratch()
  const p = paths(root)
  fs.mkdirSync(path.dirname(p.lock), { recursive: true })
  fs.writeFileSync(p.lock, String(process.pid))
  const all = [session("ses_a", { updated: T0 + 1000 })]
  const held = stubCtx(all)
  await (await load(root)).setup(held.ctx)
  await until(() => fs.existsSync(p.log) && /another process holds the lock/.test(fs.readFileSync(p.log, "utf8")))
  assert.equal(held.calls.list.length, 0)
  assert.ok(!fs.existsSync(p.stamp))

  fs.writeFileSync(p.lock, "999999")
  const stale = stubCtx(all)
  await (await load(root)).setup(stale.ctx)
  await until(complete(p.stamp))
  assert.equal(stale.calls.context.length, 1)
  assert.ok(!fs.existsSync(p.lock))
})

test("injected system prompts become isMeta user lines, plain prompts carry no flag", async () => {
  const root = scratch()
  const p = paths(root)
  const s = session("ses_meta", { updated: T0 + 1000, turns: 3 })
  s.messages[0] = { ...s.messages[0], text: "<base>\nYou are an agent inside Buzz.\n</base>\n\n<context>\nchannel: #ops\n</context>" }
  s.messages[2] = { id: s.messages[2].id, type: "synthetic", text: "generated by the host", time: s.messages[2].time }
  await (await load(root)).setup(stubCtx([s]).ctx)
  await until(complete(p.stamp))
  const users = lines(path.join(p.transcripts, "ses_meta.jsonl")).filter((l) => l.type === "user" && typeof l.message.content === "string")
  assert.equal(users.length, 3)
  assert.equal(users[0].isMeta, true)
  assert.match(users[0].message.content, /^<base>/)
  assert.match(users[0].message.content, /<context>/)
  assert.equal(users[1].isMeta, true)
  assert.equal(users[2].isMeta, undefined)
  assert.equal(users[2].message.content, "prompt 2 for ses_meta")
})

test("model-change is written on transitions only and survives a restart", async () => {
  const root = scratch()
  const p = paths(root)
  const s = session("ses_model", { updated: T0 + 1000, turns: 3 })
  s.messages[3].model = { id: "llm-large", providerID: "decent" }
  s.messages[5].model = { id: "llm-large", providerID: "decent" }
  await (await load(root)).setup(stubCtx([s]).ctx)
  await until(complete(p.stamp))
  const file = path.join(p.transcripts, "ses_model.jsonl")
  const changes = lines(file).filter((l) => l.type === "model-change")
  assert.deepEqual(
    changes.map((l) => [l.uuid, l.model]),
    [
      ["msg_a_ses_model_0-model", "decent/llm-default"],
      ["msg_a_ses_model_1-model", "decent/llm-large"],
    ],
  )
  const { ctx, ev } = stubCtx([s])
  await (await load(root, { CCCP_BACKFILL: "off" })).setup(ctx)
  const more = session("ses_model", { updated: T0 + 2000, turns: 4, modelID: "llm-large" }).messages.slice(6)
  await replay(ev, { info: s.info, messages: more })
  assert.equal(lines(file).filter((l) => l.type === "model-change").length, 2)
  assert.equal(lines(file).filter((l) => l.type === "usage").length, 4)
})

test("a step's changed files become a system patch line; none, none", async () => {
  const root = scratch()
  const p = paths(root)
  const extra = (id, t) => (t === 0 ? { snapshot: { start: "aaaa", end: "c831db25", files: ["a.txt", "b.txt"] } } : { snapshot: { start: "x", end: "y", files: [] } })
  const s = session("ses_patch", { updated: T0 + 1000, turns: 2, extra })
  await (await load(root)).setup(stubCtx([s]).ctx)
  await until(complete(p.stamp))
  const ls = lines(path.join(p.transcripts, "ses_patch.jsonl"))
  assert.deepEqual(
    ls.map((l) => l.type),
    ["user", "model-change", "assistant", "assistant", "user", "assistant", "system", "usage", "user", "assistant", "assistant", "user", "assistant", "usage"],
  )
  const patch = ls[6]
  assert.equal(patch.uuid, "msg_a_ses_patch_0-patch")
  assert.equal(patch.subtype, "patch")
  assert.equal(patch.content, "a.txt\nb.txt")
  assert.deepEqual(patch.files, ["a.txt", "b.txt"])
  assert.equal(patch.hash, "c831db25")
  assert.equal(patch.timestamp, new Date(T0 + 1000 - 50000 + 5000).toISOString())
})

const FIXTURE = new URL("../../../internal/pravda/fixtures/opencode-synthetic.jsonl", import.meta.url)

function fixtureScenario() {
  const id = "ses_harness1"
  const t0 = 1700000000000
  const info = { id, projectID: "p1", title: "harness", location: { directory: "/home/dev/proj" }, time: { created: t0, updated: t0 + 200 } }
  const tool = (callID, name, input, created, ran, completed, state) => ({ type: "tool", id: callID, name, executed: false, state: { input, ...state }, time: { created, ran, completed } })
  const tok1 = { input: 10, output: 5, reasoning: 0, cache: { read: 0, write: 0 } }
  const tok2 = { input: 1200, output: 40, reasoning: 30, cache: { read: 50, write: 0 } }
  return {
    info,
    messages: [
      { id: "msg_u1", type: "user", text: "say hi and list the dir", files: [], time: { created: t0 } },
      {
        id: "msg_a1", type: "assistant", agent: "build", model: { id: "llm-default", providerID: "decent" },
        content: [
          { type: "reasoning", text: "thinking about it", time: { created: t0 + 11, completed: t0 + 12 } },
          { type: "text", text: "Hi! Let me look." },
          tool("call_1", "shell", { command: "ls", description: "list" }, t0 + 14, t0 + 15, t0 + 16, { status: "completed", content: [{ type: "text", text: "a.txt\nb.txt" }], metadata: {} }),
          tool("call_2", "read", { path: "/nope" }, t0 + 16, t0 + 17, t0 + 18, { status: "error", error: { type: "tool.execution", message: "no such file" } }),
          { type: "text", text: "Done." },
        ],
        finish: "stop", cost: 0.0005, tokens: tok1, time: { created: t0 + 10, completed: t0 + 20 },
      },
      { id: "msg_u2", type: "user", text: "<base>\nYou are an agent operating inside Buzz.\n</base>\n\n<buzz-event>\nnew message in #ops: fix a.txt\n</buzz-event>", files: [], time: { created: t0 + 100 } },
      {
        id: "msg_a2", type: "assistant", agent: "build", model: { id: "claude-opus-5", providerID: "anthropic" },
        content: [
          tool("call_3", "edit", { path: "/home/dev/proj/a.txt", oldString: "a", newString: "b" }, t0 + 114, t0 + 115, t0 + 116, { status: "completed", content: [{ type: "text", text: "Edit applied" }], metadata: {} }),
          { type: "text", text: "Patched." },
        ],
        snapshot: { start: "0000", end: "c831db2589f913003b2a5cf93b51654dc1f58297", files: ["/home/dev/proj/a.txt"] },
        finish: "stop", cost: 0.0021, tokens: tok2, time: { created: t0 + 110, completed: t0 + 130 },
      },
    ],
  }
}

test("fixtures/opencode-synthetic.jsonl is what the live event path writes (CCCP_FIXTURE_WRITE=1 regenerates)", async () => {
  const write = FIXTURE_WRITE
  const root = scratch()
  const p = paths(root)
  const s = fixtureScenario()
  const { ctx, ev } = stubCtx([], { directory: "/home/dev/proj" })
  await (await load(root, { CCCP_BACKFILL: "off" })).setup(ctx)
  await replay(ev, s)
  const file = path.join(p.transcripts, `${s.info.id}.jsonl`)
  const got = fs.readFileSync(file, "utf8")
  assert.deepEqual(
    lines(file).map((l) => l.type),
    ["user", "model-change", "assistant", "assistant", "assistant", "user", "assistant", "user", "assistant", "usage", "user", "model-change", "assistant", "user", "assistant", "system", "usage"],
  )
  if (write) {
    fs.writeFileSync(FIXTURE, got + "\n   \nnot json\n")
    return
  }
  const want = fs
    .readFileSync(FIXTURE, "utf8")
    .split("\n")
    .filter((l) => l.trim().startsWith("{"))
  assert.deepEqual(got.split("\n").filter(Boolean), want)
})
