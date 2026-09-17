# cccp (OpenCode plugin)

`cccp.js` is a single dependency-free ESM file that runs inside OpenCode's
Bun runtime. Pairs with `cccp watch --agent opencode`.

What it does:

- Mirrors every session into `$XDG_DATA_HOME/opencode/transcripts/<sessionID>.jsonl`
  (default `~/.local/share/opencode/transcripts/`), one Claude-Code-shaped
  line per finished part: `user` lines for prompts, `assistant` lines with a
  `text` / `thinking` / `tool_use` block, and a `user` line carrying the
  `tool_result`. `uuid` is the OpenCode part id (`<partID>-result` for tool
  results), `parentUuid` chains the file, `sessionId`/`cwd`/`timestamp`
  follow the Claude shape, so pravda renders it through the normal pipeline
  and the first user message is the title. Tool names map to their Claude
  spellings (`bash` -> `Bash`, `read` -> `Read`, ...) and `filePath` /
  `oldString` / `newString` gain `file_path` / `old_string` / `new_string`
  aliases so the tool templates find them; the original keys stay. Besides
  the Claude-shaped lines it writes three synthetic ones (all with the same
  base fields, uuid and `parentUuid` chain):
  - `usage`, one per `step-finish` part, written when the message completes:
    `{"type":"usage","model":"provider/model","usage":{"input_tokens","output_tokens","cache_read_input_tokens","cache_creation_input_tokens","reasoning_tokens"},"costUSD":0.0062,"stepReason":"stop"}`.
    `output_tokens` includes OpenCode's separate `reasoning` count so the four
    Claude buckets add up to the step total; `uuid` is the step-finish part id.
    pravda drops these from the timeline and sums them in the Environment
    panel. The last assistant line of a completed message still carries
    `message.usage` as before.
  - `model-change` (`{"type":"model-change","model":"provider/model","effort":""}`,
    uuid `<messageID>-model`) whenever an assistant message's
    `providerID/modelID` differs from the previous one in the session; the
    transcript's last model is re-read at startup so a restart does not
    repeat it.
  - `system` with `subtype: "patch"` for each `patch` part: `content` lists
    the changed files one per line, `files` and `hash` (the snapshot commit)
    ride along. OpenCode stores no diff in the part, only the snapshot hash,
    so this is a file list, not a diff.
  User messages whose first text part is an injected prompt (`synthetic`
  flag, or a leading `<tag>` such as Buzz's `<base>` / `<context>`) carry
  `"isMeta": true`, so pravda files them as meta instead of a user turn and
  the title skips them.
- Runs `cccp watch hook-start --agent opencode --dir <data root>` with
  `{"session_id": ...}` on stdin the first time it sees a top-level session
  (child sessions from the `task` tool get their own transcript but no
  hook), and `hook-end` for every hooked session on `dispose` and on
  process exit. Both run synchronously (`spawnSync`, 10 s cap) so an
  instant exit cannot run `hook-end` before `hook-start` has registered
  the session; when the exit-time `hook-end` empties `sessions.json` the
  plugin waits up to 3 s for `secretary.pid` to disappear, so the daemon's
  final push completes before the process (and a bwrap sandbox around it)
  goes away. A missing `cccp` is logged once and otherwise a no-op:
  transcripts are still written. Set `CCCP_BIN` to point at a specific
  binary.
- Writes synchronously inside the event handler: `opencode run` exits the
  moment the session goes idle, and plugin event hooks are not awaited.
  Parts are appended in message order once final (`time.end` on text and
  reasoning, `completed` / `error` on tools); a message that completes with
  parts still open is flushed whole. `step-finish` and `patch` are written
  after the message's content, once it is complete (they carry no timestamp
  of their own, so they take the message's `time.completed`). Skipped on
  purpose: `step-start` and `snapshot` (only a snapshot tree hash),
  `agent` (agent selector, not content), `retry` (transport retry marker,
  no payload), `compaction` (marker only; the following message carries the
  summary text), `subtask` (the child session's prompt; the child has its
  own transcript).
- Re-reads the transcript's `uuid`s at startup, so a restarted OpenCode
  never duplicates a line, and backfills a resumed session from
  `client.session.messages` when the transcript is missing history.
- Never throws out of `event`; failures land in
  `<data root>/cccp/opencode-plugin.log`.

## Backfill

Two seconds after it loads, the plugin walks OpenCode's session history and
writes a transcript for every session that does not have one yet, so the
first launch with the plugin installed ships the whole past through the same
`transcripts/` allowlist. It never blocks the event handler and never throws.

- Listing goes through the SDK's `GET /experimental/session` with an empty
  `directory` query, which is the one route that spans every project in
  `opencode-stable.db` (the plain `session.list` is scoped to the project the
  plugin was loaded in). It pages 50 sessions at a time via `x-next-cursor`,
  includes archived and child (`task`) sessions, and each session's messages
  come from `session.messages` -- the same call and the same emit helpers the
  live path uses, so lines are the same shape: original OpenCode timestamps
  (ms epoch -> RFC3339 UTC), `cwd` from the assistant's `path.cwd` or the
  session's `directory`, uuid = part id. If the experimental route is missing
  the pass falls back to `session.list({ scope: "project" })` for the current
  project only and says so in the plugin log.
- Idempotent: the transcript's existing `uuid`s are re-read first, so a
  second pass appends nothing. Sessions live in this process, or listed in
  `<data root>/cccp/sessions.json` by another OpenCode's daemon, are skipped.
  A lock file (`cccp/backfill.lock`, pid inside, reclaimed when stale) keeps
  two OpenCode processes from backfilling the same files at once.
- Incremental: `<data root>/cccp/backfill.json` records the newest
  `time.updated` seen by a completed pass; later launches only list sessions
  updated at or after it. It lives under `cccp/`, not `transcripts/`, because
  the allowlist would otherwise ship it to every other host. A pass stops
  after ~20 s (`CCCP_BACKFILL_BUDGET_MS`) and stores a `pending` cursor, so a
  large history finishes over a few launches; SDK calls are spaced 100 ms
  apart.
- One difference from live transcripts: the last assistant line of a
  backfilled message carries `stop_reason` and `usage`, because history is
  read after the message completed; a live session's last part is written
  before OpenCode marks the message complete, so there it stays `null`.
- `CCCP_BACKFILL=all` forces a full pass that ignores the stamp (the uuid
  dedup still makes it append-only); `CCCP_BACKFILL=off` disables the pass.
  Sessions written before the `usage` / `model-change` / `patch` / `isMeta`
  lines existed do not grow them on a normal launch (the stamp skips them);
  a `CCCP_BACKFILL=all` pass appends only the lines whose uuid is new, at
  the end of each file, so those transcripts get their usage/model/patch
  lines but the `isMeta` flag stays absent from user lines already written
  (rewriting a line would break the append-only sync).
  A convenient way to run a full pass without a model call is
  `CCCP_BACKFILL=all CCCP_BACKFILL_BUDGET_MS=600000 opencode serve --port N`,
  then `curl http://127.0.0.1:N/project` once (`serve` only bootstraps its
  instance, and so loads plugins, on the first request), wait for the
  `backfill: ... complete` line in the plugin log, then stop it. No custom
  tool is registered for this on purpose: a plugin tool is sent to the model
  on every turn.
- Cost to know about: OpenCode routes `/session/:id/*` to an instance for
  that session's own directory, so the first fetch of a session from another
  project bootstraps an in-process instance for that directory (its config
  and `.opencode/` plugins load; LSPs/MCP stay lazy), about what opening
  that project does. One per distinct directory in the history, not per
  session. Reading `opencode-stable.db` directly would avoid it but was not
  chosen: the SDK enumerates history fine and stays stable across schema
  changes.
- The plugin log gets one summary line per pass:
  `backfill: scope=global sessions=N skipped=K files=F lines=L errors=E ms=T complete`.
- Test harness: `node --test plugin/opencode/test/backfill.test.mjs` (a stub
  client; no OpenCode needed). One test pins
  `internal/pravda/fixtures/opencode-synthetic.jsonl` to what the live event
  path writes; after an intentional output change regenerate it with
  `CCCP_FIXTURE_WRITE=1 node --test plugin/opencode/test/backfill.test.mjs`.

## Install

OpenCode auto-discovers `plugin/*.js` under its global config dir
(`$XDG_CONFIG_HOME/opencode`, default `~/.config/opencode`) and under a
project's `.opencode/`. Copy the file -- OpenCode's glob does not follow a
symlinked file:

```sh
mkdir -p ~/.config/opencode/plugin
cp plugin/opencode/cccp.js ~/.config/opencode/plugin/cccp.js
```

Or declare it in `opencode.json`: `"plugin": ["./plugin/cccp.js"]`
(paths resolve relative to the config file).

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
- OpenCode's own `run` wraps the prompt in quotes in its stored user part;
  the transcript keeps them.
