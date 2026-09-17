import { test } from "node:test"
import assert from "node:assert/strict"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"

const PLUGIN = new URL("../cccp.js", import.meta.url).href
const FIXTURE_WRITE = process.env.CCCP_FIXTURE_WRITE
let seq = 0
process.setMaxListeners(0)

function scratch() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "cccp-backfill-"))
}

async function load(root, env = {}) {
  for (const k of Object.keys(process.env)) if (k.startsWith("CCCP_")) delete process.env[k]
  process.env.XDG_DATA_HOME = root
  process.env.CCCP_BIN = path.join(root, "no-such-cccp")
  process.env.CCCP_BACKFILL_DELAY_MS = "0"
  Object.assign(process.env, env)
  const mod = await import(`${PLUGIN}?i=${++seq}`)
  return mod.CccpPlugin
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

function lines(file) {
  return fs
    .readFileSync(file, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l))
}

const T0 = 1788800000000

function session(id, { updated, directory = "/work/proj", projectID = "p1", turns = 1, parentID, modelID = "llm-default", extraParts = () => [] } = {}) {
  const info = {
    id,
    projectID,
    directory,
    title: `title ${id}`,
    version: "1.18.29",
    time: { created: updated - 60000, updated },
  }
  if (parentID) info.parentID = parentID
  const messages = []
  for (let t = 0; t < turns; t++) {
    const base = updated - 50000 + t * 10000
    const um = `msg_u_${id}_${t}`
    const am = `msg_a_${id}_${t}`
    messages.push({
      info: { id: um, sessionID: id, role: "user", time: { created: base }, agent: "build", model: { providerID: "decent", modelID } },
      parts: [{ id: `prt_ut_${id}_${t}`, sessionID: id, messageID: um, type: "text", text: `prompt ${t} for ${id}` }],
    })
    messages.push({
      info: {
        id: am,
        sessionID: id,
        role: "assistant",
        parentID: um,
        time: { created: base + 100, completed: base + 5000 },
        providerID: "decent",
        modelID,
        agent: "build",
        finish: "stop",
        cost: 0.0012,
        tokens: { total: 135, input: 100, output: 20, reasoning: 10, cache: { read: 5, write: 0 } },
        path: { cwd: `${directory}/sub`, root: directory },
      },
      parts: [
        { id: `prt_ss_${id}_${t}`, sessionID: id, messageID: am, type: "step-start", snapshot: "abc" },
        { id: `prt_r_${id}_${t}`, sessionID: id, messageID: am, type: "reasoning", text: "thinking...", time: { start: base + 200, end: base + 900 } },
        {
          id: `prt_tool_${id}_${t}`,
          sessionID: id,
          messageID: am,
          type: "tool",
          callID: `call_${id}_${t}`,
          tool: "bash",
          state: {
            status: "completed",
            input: { command: "echo hi", description: "say hi" },
            output: "hi\n",
            title: "echo hi",
            metadata: {},
            time: { start: base + 1000, end: base + 1500 },
          },
        },
        { id: `prt_t_${id}_${t}`, sessionID: id, messageID: am, type: "text", text: `answer ${t}`, time: { start: base + 2000, end: base + 4000 } },
        { id: `prt_sf_${id}_${t}`, sessionID: id, messageID: am, type: "step-finish", reason: "stop", cost: 0.0012, tokens: { total: 135, input: 100, output: 20, reasoning: 10, cache: { read: 5, write: 0 } } },
        ...extraParts(id, am, t, base),
      ],
    })
  }
  return { info, messages }
}

function stubClient(sessions, { global = true, messageDelay = 0, project = "p1" } = {}) {
  const calls = { list: [], messages: [] }
  const desc = (rows) => [...rows].sort((a, b) => b.time.updated - a.time.updated)
  const client = {
    session: {
      async list(opts) {
        calls.list.push(opts)
        const q = opts?.query ?? {}
        if (opts?.url === "/experimental/session") {
          if (!global) return { error: { name: "NotFoundError", data: { message: "no such route" } }, response: { status: 404, headers: new Map() } }
          let rows = desc(sessions.map((s) => s.info))
          if (q.start) rows = rows.filter((s) => s.time.updated >= q.start)
          if (q.cursor) rows = rows.filter((s) => s.time.updated < q.cursor)
          const limit = q.limit ?? 100
          const page = rows.slice(0, limit)
          const headers = new Map()
          if (rows.length > limit && page.length) headers.set("x-next-cursor", String(page[page.length - 1].time.updated))
          return { data: page, response: { status: 200, headers } }
        }
        let rows = desc(sessions.map((s) => s.info)).filter((s) => s.projectID === project)
        if (q.start) rows = rows.filter((s) => s.time.updated >= q.start)
        return { data: rows.slice(0, q.limit ?? 100), response: { status: 200, headers: new Map() } }
      },
      async messages({ path: { id } }) {
        calls.messages.push(id)
        if (messageDelay) await new Promise((r) => setTimeout(r, messageDelay))
        const s = sessions.find((x) => x.info.id === id)
        return { data: s ? s.messages : [], response: { status: 200, headers: new Map() } }
      },
    },
  }
  return { client, calls }
}

test("full pass writes every session across projects, pages the listing, preserves timestamps and cwd", async () => {
  const root = scratch()
  const p = paths(root)
  const all = []
  for (let i = 0; i < 55; i++) all.push(session(`ses_${String(i).padStart(3, "0")}`, { updated: T0 + i * 1000, projectID: i % 2 ? "p1" : "p2", directory: i % 2 ? "/work/a" : "/work/b" }))
  all.push(session("ses_rich", { updated: T0 + 100000, turns: 2, directory: "/work/rich", projectID: "p3" }))
  const { client, calls } = stubClient(all)
  const plugin = await load(root)
  await plugin({ client, directory: "/work/a" })
  const stamp = await until(complete(p.stamp), 20000)

  assert.equal(stamp.updated, T0 + 100000)
  assert.equal(stamp.scope, "global")
  assert.equal(fs.readdirSync(p.transcripts).length, 56)
  assert.equal(calls.list.length, 2)
  assert.equal(calls.list[0].url, "/experimental/session")
  assert.equal(calls.list[0].query.directory, "")
  assert.equal(calls.list[0].query.archived, true)
  assert.equal(calls.list[0].query.start, undefined)
  assert.equal(calls.list[1].query.cursor, T0 + 6000)
  assert.equal(calls.messages.length, 56)
  assert.ok(!fs.existsSync(p.lock))

  const rich = lines(path.join(p.transcripts, "ses_rich.jsonl"))
  assert.deepEqual(
    rich.map((l) => l.type),
    ["user", "model-change", "assistant", "assistant", "user", "assistant", "usage", "user", "assistant", "assistant", "user", "assistant", "usage"],
  )
  const [u, mc, think, tool, result, text, usage] = rich
  assert.equal(u.uuid, "msg_u_ses_rich_0")
  assert.equal(u.parentUuid, null)
  assert.equal(u.message.content, "prompt 0 for ses_rich")
  assert.equal(u.timestamp, new Date(T0 + 100000 - 50000).toISOString())
  assert.equal(u.cwd, "/work/rich")
  assert.equal(u.sessionId, "ses_rich")
  assert.equal(u.agent, "opencode")
  assert.equal(u.version, "1.18.29")
  assert.equal(mc.uuid, "msg_a_ses_rich_0-model")
  assert.equal(mc.model, "decent/llm-default")
  assert.equal(mc.effort, "")
  assert.equal(mc.timestamp, new Date(T0 + 100000 - 50000 + 100).toISOString())
  assert.equal(think.uuid, "prt_r_ses_rich_0")
  assert.equal(think.parentUuid, mc.uuid)
  assert.equal(think.message.content[0].type, "thinking")
  assert.equal(think.cwd, "/work/rich/sub")
  assert.equal(think.timestamp, new Date(T0 + 100000 - 50000 + 200).toISOString())
  assert.equal(tool.message.content[0].type, "tool_use")
  assert.equal(tool.message.content[0].name, "Bash")
  assert.equal(tool.message.content[0].id, "call_ses_rich_0")
  assert.equal(result.uuid, "prt_tool_ses_rich_0-result")
  assert.equal(result.message.content[0].tool_use_id, "call_ses_rich_0")
  assert.equal(result.message.content[0].content, "hi\n")
  assert.equal(result.toolUseResult.title, "echo hi")
  assert.equal(text.message.content[0].text, "answer 0")
  assert.equal(text.message.stop_reason, "stop")
  assert.equal(text.message.usage.input_tokens, 100)
  assert.equal(text.message.model, "decent/llm-default")
  assert.equal(usage.uuid, "prt_sf_ses_rich_0")
  assert.equal(usage.model, "decent/llm-default")
  assert.equal(usage.timestamp, new Date(T0 + 100000 - 50000 + 5000).toISOString())
  assert.deepEqual(usage.usage, { input_tokens: 100, output_tokens: 30, cache_read_input_tokens: 5, cache_creation_input_tokens: 0, reasoning_tokens: 10 })
  assert.equal(usage.costUSD, 0.0012)
  assert.equal(usage.stepReason, "stop")
  assert.equal(usage.message, undefined)
  for (let i = 1; i < rich.length; i++) assert.equal(rich[i].parentUuid, rich[i - 1].uuid)
  assert.equal(new Set(rich.map((l) => l.uuid)).size, rich.length)
})

test("backfilled bytes equal the live event path for the same messages", async () => {
  const s = session("ses_same", { updated: T0 + 5000, turns: 1, directory: "/work/same" })

  const rootA = scratch()
  const pa = paths(rootA)
  const pluginA = await load(rootA)
  await pluginA({ client: stubClient([s]).client, directory: "/work/same" })
  await until(complete(pa.stamp))

  const rootB = scratch()
  const pb = paths(rootB)
  const pluginB = await load(rootB, { CCCP_BACKFILL: "off" })
  const hooks = await pluginB({ client: { session: {} }, directory: "/work/same" })
  const ev = (type, properties) => hooks.event({ event: { type, properties } })
  await ev("session.created", { info: s.info })
  const [um, am] = s.messages
  await ev("message.updated", { info: um.info })
  for (const part of um.parts) await ev("message.part.updated", { part })
  await ev("message.updated", { info: { ...am.info, time: { created: am.info.time.created } } })
  for (const part of am.parts) await ev("message.part.updated", { part })
  await ev("message.updated", { info: am.info })
  await ev("session.idle", { sessionID: s.info.id })

  const a = lines(path.join(pa.transcripts, "ses_same.jsonl"))
  const b = lines(path.join(pb.transcripts, "ses_same.jsonl"))
  assert.equal(a.length, 7)
  assert.equal(b.length, 7)
  assert.equal(a.at(-1).type, "usage")
  assert.equal(a.at(-2).message.stop_reason, "stop")
  assert.equal(a.at(-2).message.usage.output_tokens, 20)
  assert.equal(b.at(-2).message.stop_reason, null)
  assert.equal(b.at(-2).message.usage, undefined)
  for (const l of [a.at(-2), b.at(-2)]) {
    delete l.message.stop_reason
    delete l.message.usage
  }
  assert.equal(a.map((l) => JSON.stringify(l)).join("\n"), b.map((l) => JSON.stringify(l)).join("\n"))
  assert.ok(!fs.existsSync(pb.stamp))
})

test("second launch is incremental and writes nothing for unchanged sessions", async () => {
  const root = scratch()
  const p = paths(root)
  const all = [session("ses_a", { updated: T0 + 1000 }), session("ses_b", { updated: T0 + 2000 }), session("ses_c", { updated: T0 + 3000 })]

  const first = stubClient(all)
  await (await load(root))({ client: first.client, directory: "/work/proj" })
  const stamp1 = await until(complete(p.stamp))
  const snapshot = Object.fromEntries(fs.readdirSync(p.transcripts).map((f) => [f, fs.readFileSync(path.join(p.transcripts, f), "utf8")]))

  const second = stubClient(all)
  await (await load(root))({ client: second.client, directory: "/work/proj" })
  const stamp2 = await until(() => {
    const s = complete(p.stamp)()
    return s && s.at !== stamp1.at ? s : null
  })
  assert.equal(second.calls.list[0].query.start, T0 + 3000)
  assert.deepEqual(second.calls.messages, ["ses_c"])
  assert.equal(stamp2.updated, T0 + 3000)
  for (const [f, body] of Object.entries(snapshot)) assert.equal(fs.readFileSync(path.join(p.transcripts, f), "utf8"), body)

  all.push(session("ses_d", { updated: T0 + 9000 }))
  all[0].info.time.updated = T0 + 8000
  all[0].messages.push(...session("ses_a", { updated: T0 + 8000, turns: 2 }).messages.slice(2))
  const third = stubClient(all)
  await (await load(root))({ client: third.client, directory: "/work/proj" })
  const stamp3 = await until(() => {
    const s = complete(p.stamp)()
    return s && s.at !== stamp2.at ? s : null
  })
  assert.deepEqual(third.calls.messages.sort(), ["ses_a", "ses_c", "ses_d"])
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
  const { client, calls } = stubClient(all)
  const hooks = await (await load(root))({ client, directory: "/work/proj" })
  await hooks.event({ event: { type: "session.created", properties: { info: all[1].info } } })
  const stamp = await until(complete(p.stamp))
  assert.deepEqual(calls.messages, ["ses_old"])
  assert.deepEqual(fs.readdirSync(p.transcripts), ["ses_old.jsonl"])
  assert.equal(stamp.updated, T0 + 3000)
})

test("time budget cuts the pass, the stamp records the cursor, the next launch resumes without duplicates", async () => {
  const root = scratch()
  const p = paths(root)
  const all = []
  for (let i = 0; i < 10; i++) all.push(session(`ses_${i}`, { updated: T0 + i * 1000 }))

  const first = stubClient(all, { messageDelay: 40 })
  await (await load(root, { CCCP_BACKFILL_BUDGET_MS: "100" }))({ client: first.client, directory: "/work/proj" })
  const stamp1 = await until(() => {
    const s = readJSON(p.stamp)
    return s?.pending ? s : null
  })
  const done1 = fs.readdirSync(p.transcripts)
  assert.ok(done1.length >= 1 && done1.length < 10, `wrote ${done1.length}`)
  assert.equal(stamp1.updated, 0)
  assert.equal(stamp1.pending.floor, null)
  assert.equal(stamp1.pending.cursor, T0 + (10 - done1.length) * 1000)
  assert.equal(stamp1.pending.newest, T0 + 9000)
  assert.ok(!fs.existsSync(p.lock))

  const second = stubClient(all)
  await (await load(root))({ client: second.client, directory: "/work/proj" })
  const stamp2 = await until(complete(p.stamp))
  assert.equal(second.calls.list[0].query.cursor, stamp1.pending.cursor)
  assert.equal(second.calls.list[0].query.start, undefined)
  assert.equal(second.calls.messages.length, 10 - done1.length)
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
  const { client, calls } = stubClient([session("ses_x", { updated: T0 })])
  await (await load(root, { CCCP_BACKFILL: "off" }))({ client, directory: "/work/proj" })
  await new Promise((r) => setTimeout(r, 150))
  assert.equal(calls.list.length, 0)
  assert.ok(!fs.existsSync(p.stamp))
  assert.ok(!fs.existsSync(p.transcripts))
})

test("CCCP_BACKFILL=all ignores the stamp and re-walks everything", async () => {
  const root = scratch()
  const p = paths(root)
  const all = [session("ses_a", { updated: T0 + 1000 }), session("ses_b", { updated: T0 + 2000 })]
  await (await load(root))({ client: stubClient(all).client, directory: "/work/proj" })
  const stamp1 = await until(complete(p.stamp))
  const { client, calls } = stubClient(all)
  await (await load(root, { CCCP_BACKFILL: "all" }))({ client, directory: "/work/proj" })
  await until(() => {
    const s = complete(p.stamp)()
    return s && s.at !== stamp1.at ? s : null
  })
  assert.equal(calls.list[0].query.start, undefined)
  assert.deepEqual(calls.messages.sort(), ["ses_a", "ses_b"])
  for (const f of fs.readdirSync(p.transcripts)) assert.equal(lines(path.join(p.transcripts, f)).length, 7)
})

test("falls back to the project-scoped listing when the global route is missing", async () => {
  const root = scratch()
  const p = paths(root)
  const all = [session("ses_here", { updated: T0 + 1000, projectID: "p1" }), session("ses_there", { updated: T0 + 2000, projectID: "p2" })]
  const { client, calls } = stubClient(all, { global: false })
  await (await load(root))({ client, directory: "/work/proj" })
  const stamp = await until(complete(p.stamp))
  assert.equal(stamp.scope, "project")
  assert.equal(calls.list.length, 2)
  assert.equal(calls.list[0].url, "/experimental/session")
  assert.equal(calls.list[1].url, undefined)
  assert.equal(calls.list[1].query.scope, "project")
  assert.deepEqual(fs.readdirSync(p.transcripts), ["ses_here.jsonl"])
  assert.match(fs.readFileSync(p.log, "utf8"), /falling back to this project/)
})

test("a live lock skips the pass, a stale lock is reclaimed", async () => {
  const root = scratch()
  const p = paths(root)
  fs.mkdirSync(path.dirname(p.lock), { recursive: true })
  fs.writeFileSync(p.lock, String(process.pid))
  const all = [session("ses_a", { updated: T0 + 1000 })]
  const held = stubClient(all)
  await (await load(root))({ client: held.client, directory: "/work/proj" })
  await until(() => fs.existsSync(p.log) && /another process holds the lock/.test(fs.readFileSync(p.log, "utf8")))
  assert.equal(held.calls.list.length, 0)
  assert.ok(!fs.existsSync(p.stamp))

  fs.writeFileSync(p.lock, "999999")
  const stale = stubClient(all)
  await (await load(root))({ client: stale.client, directory: "/work/proj" })
  await until(complete(p.stamp))
  assert.equal(stale.calls.messages.length, 1)
  assert.ok(!fs.existsSync(p.lock))
})

test("injected system prompts become isMeta user lines, plain prompts carry no flag", async () => {
  const root = scratch()
  const p = paths(root)
  const s = session("ses_meta", { updated: T0 + 1000, turns: 2 })
  s.messages[0].parts = [
    { id: "prt_base", sessionID: "ses_meta", messageID: s.messages[0].info.id, type: "text", text: "<base>\nYou are an agent inside Buzz.\n</base>" },
    { id: "prt_ctx", sessionID: "ses_meta", messageID: s.messages[0].info.id, type: "text", text: "<context>\nchannel: #ops\n</context>" },
  ]
  s.messages[2].parts = [{ id: "prt_syn", sessionID: "ses_meta", messageID: s.messages[2].info.id, type: "text", text: "generated by the host", synthetic: true }]
  s.messages.push(...session("ses_meta", { updated: T0 + 1000, turns: 3 }).messages.slice(4))
  await (await load(root))({ client: stubClient([s]).client, directory: "/work/proj" })
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
  s.messages[3].info.modelID = "llm-large"
  s.messages[5].info.modelID = "llm-large"
  await (await load(root))({ client: stubClient([s]).client, directory: "/work/proj" })
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
  const hooks = await (await load(root, { CCCP_BACKFILL: "off" }))({ client: { session: {} }, directory: "/work/proj" })
  const more = session("ses_model", { updated: T0 + 2000, turns: 4, modelID: "llm-large" }).messages.slice(6)
  await hooks.event({ event: { type: "session.created", properties: { info: s.info } } })
  for (const m of more) {
    await hooks.event({ event: { type: "message.updated", properties: { info: m.info } } })
    for (const part of m.parts) await hooks.event({ event: { type: "message.part.updated", properties: { part } } })
  }
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_model" } } })
  assert.equal(lines(file).filter((l) => l.type === "model-change").length, 2)
  assert.equal(lines(file).filter((l) => l.type === "usage").length, 4)
})

test("patch parts become system lines listing the files; empty ones are skipped", async () => {
  const root = scratch()
  const p = paths(root)
  const extraParts = (id, am, t) => [
    { id: `prt_patch_${t}`, sessionID: id, messageID: am, type: "patch", hash: "c831db25", files: ["/work/proj/a.txt", "/work/proj/b.txt"] },
    { id: `prt_patch_empty_${t}`, sessionID: id, messageID: am, type: "patch", hash: "deadbeef", files: [] },
  ]
  const s = session("ses_patch", { updated: T0 + 1000, extraParts })
  await (await load(root))({ client: stubClient([s]).client, directory: "/work/proj" })
  await until(complete(p.stamp))
  const ls = lines(path.join(p.transcripts, "ses_patch.jsonl"))
  assert.deepEqual(
    ls.map((l) => l.type),
    ["user", "model-change", "assistant", "assistant", "user", "assistant", "usage", "system"],
  )
  const patch = ls.at(-1)
  assert.equal(patch.uuid, "prt_patch_0")
  assert.equal(patch.subtype, "patch")
  assert.equal(patch.content, "/work/proj/a.txt\n/work/proj/b.txt")
  assert.deepEqual(patch.files, ["/work/proj/a.txt", "/work/proj/b.txt"])
  assert.equal(patch.hash, "c831db25")
  assert.equal(patch.timestamp, new Date(T0 + 1000 - 50000 + 5000).toISOString())
  assert.ok(!ls.some((l) => l.uuid === "prt_patch_empty_0"))
})

const FIXTURE = new URL("../../../internal/pravda/fixtures/opencode-synthetic.jsonl", import.meta.url)

function fixtureScenario() {
  const id = "ses_harness1"
  const t0 = 1700000000000
  const info = { id, projectID: "p1", directory: "/home/dev/proj", title: "harness", version: "1.18.29", time: { created: t0, updated: t0 + 200 } }
  const user = (mid, ts, parts) => ({ info: { id: mid, sessionID: id, role: "user", time: { created: ts } }, parts })
  const text = (pid, mid, t) => ({ id: pid, sessionID: id, messageID: mid, type: "text", text: t })
  const assistant = (mid, parentID, providerID, modelID, created, completed, tokens, parts) => ({
    info: { id: mid, sessionID: id, role: "assistant", parentID, time: { created, completed }, providerID, modelID, agent: "build", finish: "stop", cost: 0, tokens, path: { cwd: "/home/dev/proj", root: "/home/dev/proj" } },
    parts,
  })
  const tool = (pid, mid, callID, name, input, start, end, extra) => ({
    id: pid, sessionID: id, messageID: mid, type: "tool", callID, tool: name,
    state: { input, time: { start, end }, ...extra },
  })
  const tok1 = { total: 15, input: 10, output: 5, reasoning: 0, cache: { read: 0, write: 0 } }
  const tok2 = { total: 1320, input: 1200, output: 40, reasoning: 30, cache: { read: 50, write: 0 } }
  return {
    info,
    messages: [
      user("msg_u1", t0, [text("prt_u1", "msg_u1", "say hi and list the dir")]),
      assistant("msg_a1", "msg_u1", "decent", "llm-default", t0 + 10, t0 + 20, tok1, [
        { id: "prt_ss1", sessionID: id, messageID: "msg_a1", type: "step-start", snapshot: "0000" },
        { id: "prt_r1", sessionID: id, messageID: "msg_a1", type: "reasoning", text: "thinking about it", time: { start: t0 + 11, end: t0 + 12 } },
        { ...text("prt_t1", "msg_a1", "Hi! Let me look."), time: { start: t0 + 13, end: t0 + 14 } },
        tool("prt_c1", "msg_a1", "call_1", "bash", { command: "ls", description: "list" }, t0 + 15, t0 + 16, { status: "completed", output: "a.txt\nb.txt", title: "ls", metadata: {} }),
        tool("prt_c2", "msg_a1", "call_2", "read", { filePath: "/nope" }, t0 + 17, t0 + 18, { status: "error", error: "no such file" }),
        { ...text("prt_t2", "msg_a1", "Done."), time: { start: t0 + 19, end: t0 + 20 } },
        { id: "prt_sf1", sessionID: id, messageID: "msg_a1", type: "step-finish", reason: "stop", cost: 0.0005, tokens: tok1 },
      ]),
      user("msg_u2", t0 + 100, [text("prt_u2", "msg_u2", "<base>\nYou are an agent operating inside Buzz.\n</base>"), text("prt_u2b", "msg_u2", "<buzz-event>\nnew message in #ops: fix a.txt\n</buzz-event>")]),
      assistant("msg_a2", "msg_u2", "anthropic", "claude-opus-5", t0 + 110, t0 + 130, tok2, [
        { id: "prt_ss2", sessionID: id, messageID: "msg_a2", type: "step-start", snapshot: "1111" },
        tool("prt_c3", "msg_a2", "call_3", "edit", { filePath: "/home/dev/proj/a.txt", oldString: "a", newString: "b" }, t0 + 115, t0 + 116, { status: "completed", output: "Edit applied", title: "a.txt", metadata: {} }),
        { id: "prt_p1", sessionID: id, messageID: "msg_a2", type: "patch", hash: "c831db2589f913003b2a5cf93b51654dc1f58297", files: ["/home/dev/proj/a.txt"] },
        { ...text("prt_t3", "msg_a2", "Patched."), time: { start: t0 + 120, end: t0 + 121 } },
        { id: "prt_sf2", sessionID: id, messageID: "msg_a2", type: "step-finish", reason: "stop", cost: 0.0021, tokens: tok2 },
      ]),
    ],
  }
}

test("fixtures/opencode-synthetic.jsonl is what the live event path writes (CCCP_FIXTURE_WRITE=1 regenerates)", async () => {
  const root = scratch()
  const p = paths(root)
  const s = fixtureScenario()
  const hooks = await (await load(root, { CCCP_BACKFILL: "off" }))({ client: { session: {} }, directory: "/home/dev/proj" })
  const ev = (type, properties) => hooks.event({ event: { type, properties } })
  await ev("session.created", { info: s.info })
  for (const m of s.messages) {
    if (m.info.role === "user") {
      await ev("message.updated", { info: m.info })
      for (const part of m.parts) await ev("message.part.updated", { part })
      continue
    }
    await ev("message.updated", { info: { ...m.info, time: { created: m.info.time.created } } })
    for (const part of m.parts) await ev("message.part.updated", { part })
    await ev("message.updated", { info: m.info })
  }
  await ev("session.idle", { sessionID: s.info.id })
  const got = fs.readFileSync(path.join(p.transcripts, `${s.info.id}.jsonl`), "utf8")
  assert.deepEqual(
    lines(path.join(p.transcripts, `${s.info.id}.jsonl`)).map((l) => l.type),
    ["user", "model-change", "assistant", "assistant", "assistant", "user", "assistant", "user", "assistant", "usage", "user", "model-change", "assistant", "user", "assistant", "system", "usage"],
  )
  if (FIXTURE_WRITE) {
    fs.writeFileSync(FIXTURE, got + "\n   \nnot json\n")
    return
  }
  const want = fs
    .readFileSync(FIXTURE, "utf8")
    .split("\n")
    .filter((l) => l.trim().startsWith("{"))
  assert.deepEqual(got.split("\n").filter(Boolean), want)
})
