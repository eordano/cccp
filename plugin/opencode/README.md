# cccp (OpenCode plugin)

`cccp.js` is a single dependency-free ESM file that runs inside OpenCode's
Bun runtime. Pairs with `cccp watch --agent opencode`. It targets the
OpenCode 2.x plugin API (`export default { id: "cccp", setup(ctx) }`, events
from `ctx.event.subscribe()`); OpenCode 1.x plugins (`export const X =
async ({ client }) => ({ event })`) are rejected by 2.x with
`PluginModule.LoadError ... Missing key ["default"]`, and this file does not
load on 1.x.

What it does:

- Mirrors every session into `$XDG_DATA_HOME/opencode/transcripts/<sessionID>.jsonl`
  (default `~/.local/share/opencode/transcripts/`), one Claude-Code-shaped
  line per finished piece of content: `user` lines for prompts, `assistant`
  lines with a `text` / `thinking` / `tool_use` block, and a `user` line
  carrying the `tool_result`. `parentUuid` chains the file,
  `sessionId`/`cwd`/`timestamp` follow the Claude shape, so pravda renders it
  through the normal pipeline and the first user message is the title.
  OpenCode 2.x has no part ids, so the uuids are derived from the assistant
  message (= one model step) id: `<msgID>-text-<n>` and
  `<msgID>-reasoning-<n>` (n = OpenCode's per-step ordinal),
  `<msgID>-tool-<callID>` and `<msgID>-tool-<callID>-result`; a user line's
  uuid is its message id. Tool names map to their Claude spellings
  (`shell`/`bash` -> `Bash`, `read` -> `Read`, `subagent`/`task` -> `Agent`,
  `todo` -> `TodoWrite`, ...); `filePath` / `oldString` / `newString` gain
  `file_path` / `old_string` / `new_string` aliases and `path` gains
  `file_path` for `read`/`write`/`edit`, so the tool templates find them;
  the original keys stay. A tool result's `content` is the text of its
  content items (`[image]`-style placeholders for the others), an error's is
  the error message. Besides the Claude-shaped lines it writes three
  synthetic ones (same base fields and `parentUuid` chain):
  - `usage`, one per step (uuid `<msgID>-usage`), written when the step ends:
    `{"type":"usage","model":"provider/model","usage":{"input_tokens","output_tokens","cache_read_input_tokens","cache_creation_input_tokens","reasoning_tokens"},"costUSD":0.0062,"stepReason":"stop"}`.
    `output_tokens` includes OpenCode's separate `reasoning` count so the four
    Claude buckets add up to the step total. pravda drops these from the
    timeline and sums them in the Environment panel. The step's last
    assistant line also carries `message.usage` and `stop_reason`.
  - `model-change` (`{"type":"model-change","model":"provider/model","effort":""}`,
    uuid `<msgID>-model`) whenever a step's `providerID/id` differs from the
    previous one in the session; the transcript's last model is re-read at
    startup so a restart does not repeat it.
  - `system` with `subtype: "patch"` (uuid `<msgID>-patch`) when a step
    changed files: `content` lists them one per line, `files` and `hash`
    (the end snapshot) ride along. It is a file list, not a diff.
  A failed step gets an assistant `Error: <type>: <message>` line
  (`<msgID>-error`). User lines that are injected prompts (OpenCode
  `synthetic` messages, such as loaded instruction files or synthetic inbox
  items, or a prompt starting with a `<tag>` such as Buzz's `<base>` /
  `<context>`) carry `"isMeta": true`, so pravda files them as meta instead
  of a user turn and the title skips them. Attached files show as
  `[file: <name>]` after the prompt text.
- Runs `cccp watch hook-start --agent opencode --dir <data root>` with
  `{"session_id": ...}` on stdin the first time it sees a top-level session
  (child sessions from the `subagent` tool get their own transcript, with
  `parentSessionId`, but no hook), and `hook-end` for every hooked session
  on process exit. Both run synchronously (`spawnSync`, 10 s cap); when the
  exit-time `hook-end` empties `sessions.json` the plugin waits up to 3 s for
  `secretary.pid` to disappear, so the daemon's final push completes before
  the process (and a sandbox around it) goes away. A missing `cccp` is
  logged once and otherwise a no-op: transcripts are still written. Set
  `CCCP_BIN` to point at a specific binary.
- Where it runs: OpenCode 2.x plugins run in the *server* process. By default
  the CLI and TUI attach to a shared background service (`opencode serve
  --service`, detached, it outlives the command; its registry is
  `$XDG_STATE_HOME/opencode/service-<channel>.json`), so `hook-end` fires
  when that service stops, not when `opencode run` returns; the cccp daemon
  keeps syncing in between. With `--standalone`, or `{"disabled": true}` in
  `$XDG_CONFIG_HOME/opencode/service-<channel>.json`, each command gets a
  private `opencode serve --stdio` child that is SIGTERMed when the command
  exits; OpenCode then runs plugin cleanups and the `exit` handler, so
  `hook-end` runs per command as in 1.x.
- Writes synchronously inside the event loop (`appendFileSync`), content in
  step order: an item is written once final (`session.text.ended`,
  `session.reasoning.ended`, `session.tool.success` / `.failed`), except the
  step's last item, which waits for `session.step.ended` so it can carry
  `stop_reason` and `usage`. `session.execution.succeeded` / `.failed` /
  `.interrupted` (2.x has no `session.idle`) and process exit flush whatever
  is still open, a tool without a result as an empty `tool_result`.
  Skipped on purpose: compaction (the summary is OpenCode's, the transcript
  keeps the turns), shell (`!`) messages, agent/model/location switch
  markers.
- Plugins are instantiated per OpenCode location (directory) but share one
  event bus, so the state lives on `globalThis` per data root and every event
  id is handled once.
- Startup race: the server loads plugins lazily when the first request for a
  location arrives, which is usually the request that creates the session, so
  `session.created` (and sometimes the first prompt) can go by before the
  subscription starts. A session first seen without `session.created` is
  caught up: `ctx.session.get` supplies its directory, parent and title,
  `ctx.session.context` the messages so far (the session's events are queued
  meanwhile and replayed after). The same path covers a session resumed from
  an earlier run.
- Re-reads the transcript's `uuid`s at startup, so a restarted OpenCode
  never duplicates a line.
- Never throws out of the event loop; failures land in
  `<data root>/cccp/opencode-plugin.log`.

## Backfill

Two seconds after it loads, the plugin walks OpenCode's session history and
writes a transcript for every session that does not have one yet, so the
first launch with the plugin installed ships the past through the same
`transcripts/` allowlist. It never blocks the event loop and never throws.

- Listing is `ctx.session.list({ limit: 50, cursor })`: without a
  `directory` or `project` filter it spans every project in the database,
  newest `time.updated` first, archived and child sessions included. Each
  session's messages come from `ctx.session.context`, the only message
  access the 2.x plugin context offers. It returns the messages after the
  session's last compaction, so **a compacted session backfills only its
  post-compaction window** (the live path records everything). Lines go
  through the same emitters as the live path and are byte-identical to it
  except the `ai-title` timestamp; `version` is the running OpenCode's
  (`ctx.app.version`), since 2.x session info no longer stores one.
- Idempotent: the transcript's existing `uuid`s are re-read first, so a
  second pass appends nothing. Sessions live in this process, or listed in
  `<data root>/cccp/sessions.json` by another OpenCode's daemon, are skipped.
  A lock file (`cccp/backfill.lock`, pid inside, reclaimed when stale) keeps
  two OpenCode processes from backfilling the same files at once.
- Incremental: `<data root>/cccp/backfill.json` records the newest
  `time.updated` seen by a completed pass; later passes stop at the first
  session updated before it (sessions updated exactly then are re-listed).
  It lives under `cccp/`, not `transcripts/`, because the allowlist would
  otherwise ship it to every other host. A pass stops after ~20 s
  (`CCCP_BACKFILL_BUDGET_MS`) and stores a `pending` cursor (the id and
  `time.updated` of the last session done), so a large history finishes over
  a few launches; calls are spaced 100 ms apart. A stamp written by the 1.x
  plugin keeps working: its `updated` floor applies, its numeric pending
  cursor is ignored (the pass restarts from the newest session).
- `CCCP_BACKFILL=all` forces a full pass that ignores the stamp (the uuid
  dedup still makes it append-only); `CCCP_BACKFILL=off` disables the pass.
  Sessions recorded by the 1.x plugin used part ids as uuids, so a forced
  pass over them appends their post-compaction window again under the 2.x
  uuids; don't force one on a host with 1.x transcripts unless that is
  wanted.
- The plugin log gets one summary line per pass:
  `backfill: scope=global sessions=N skipped=K files=F lines=L errors=E ms=T complete`.
- Test harness: `node --test plugin/opencode/test/backfill.test.mjs` (a stub
  plugin context with a scripted event stream; no OpenCode needed). One test
  pins `internal/pravda/fixtures/opencode-synthetic.jsonl` to what the live
  event path writes; after an intentional output change regenerate it with
  `CCCP_FIXTURE_WRITE=1 node --test plugin/opencode/test/backfill.test.mjs`.

## Install

OpenCode 2.x auto-discovers `{plugin,plugins}/*` under its global config dir
(`$XDG_CONFIG_HOME/opencode`, default `~/.config/opencode`) and under a
project's `.opencode/`: a `.js`/`.ts` file, a directory with an `index.js`
(or `server.js`), or a symlink to either. Copy the file:

```sh
mkdir -p ~/.config/opencode/plugin
cp plugin/opencode/cccp.js ~/.config/opencode/plugin/cccp.js
```

A path in `opencode.json`'s `plugin` list must now be a directory (a file
path is refused with "configured plugin path must be a directory"), so use
the plugin dir, or point the config at a directory holding `index.js`.

Then give cccp a server, in the agent's own state dir:

```sh
mkdir -p ~/.local/share/opencode/cccp
cat > ~/.local/share/opencode/cccp/config.json <<'JSON'
{ "url": "https://sync.example", "token": "...", "agent": "opencode" }
JSON
```

Transcripts land server-side as `agent/opencode/transcripts/<id>.jsonl`.
The sandboxed-agents `opencode` package seeds the plugin and puts `cccp`
on the sandbox PATH when built with a `cccp` package.

## Troubleshooting

- Daemon log: `~/.local/share/opencode/cccp/secretary.log`
- Plugin log: `~/.local/share/opencode/cccp/opencode-plugin.log`
- Status: `cccp watch status --agent opencode`
- Stop: `cccp watch stop --agent opencode`
- A plugin that fails to load does not stop OpenCode; the server logs
  `failed to load plugin` (run the service in the foreground with
  `opencode serve --print-logs`, or look in `$XDG_DATA_HOME/opencode/log/`).
- The background service keeps the plugin file it loaded; editing the file
  makes it reload (cleanup, then setup again), and stopping the service
  (`kill` the pid in the service registry) picks up a new environment.
- OpenCode's own `run` wraps the prompt in quotes in its stored user text;
  the transcript keeps them.
