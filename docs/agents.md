# cccp agent integrations

Per-agent internals behind `--agent`: plugin hooks, converters, allowlists,
backfill and local testing. The shared invariants are in `../AGENTS.md`.

## Hermes

- Hermes keeps sessions in `state.db` (sqlite), so nothing under
  `~/.hermes` is line-syncable as-is. `plugin/hermes/` is a native Hermes
  plugin (`plugin.yaml` + `__init__.py:register(ctx)`) that mirrors each
  session as Claude-shaped jsonl at `transcripts/<session_id>.jsonl`;
  `convert_hermes.go` is therefore an identity converter and
  `fixtures/hermes-session.jsonl` pins the shape the plugin emits (a
  change to the plugin's output must update the fixture).
- Hook mapping: `pre_llm_call` -> `user` text line (once per turn, deduped
  on `(text, #user messages in history)`); `post_tool_call` ->
  `assistant`/`tool_use` + `user`/`tool_result`, keyed on Hermes'
  `tool_call_id`; `post_llm_call` -> final `assistant` text (+ `thinking`
  from the last assistant `reasoning`). `on_session_end` fires per turn
  and is ignored; `on_session_finalize` is the real end and runs
  `hook-end`. `hook-start` runs lazily on the first hook seen for a
  session, not only on `on_session_start`, because resumed sessions never
  fire the latter.
- Every hook is fail-open (log + return None); `cccp` missing from PATH
  is a no-op; subprocesses get a 10 s timeout and `HERMES_HOME` set to the
  home the plugin writes to, so the profile's `DefaultDir` resolves the
  same dir.
- Profile allowlist: `transcripts/`, `memories/`, `SOUL.md`.
  `skills/` is deliberately out: Hermes seeds it with ~400 bundled files on
  first run, so it is vendor content, not user state. Never `state.db*`,
  `.env`, `auth.json`, `*_cache.json`.
- `config.yaml` left the allowlist on 2026-10-28 (owner's call). Every home
  has its own (providers, model, enabled plugins), the class is privileged so
  no host ever applied one, and the fleet's homes in `agent/hermes/`
  replaced each other's version every minute: 12.8 k versions from 10-27 to
  10-28. The versions already on the server stay as history and are no
  longer updated once every host runs a binary without it. Until then an
  older binary adds a version only when its own copy changes (ingest skips
  re-sends, see **Sync** in `../AGENTS.md`), except where two homes share a
  host id: a host's `~/.hermes` and `~/.hermes-local` alternate until it runs
  the new binary.
- The profile's `Exclude` keeps the backfill's own files out of sync even
  though they sit under `transcripts/`: `.backfill.json`, its `.tmp` and
  `.backfill.lock` (the plugin moved them to `cccp/` on 2026-10-28, see
  below; an older plugin still writes them there). Push neither sends nor
  tombstones an excluded path, and pull neither writes nor removes one, so
  a stamp that an older binary still pushes never reaches an upgraded home.
  Before, `.backfill.json` shipped as a transcript-class blob: 7.1 k
  versions from the fleet's homes over the same day, and a pulling host
  would have applied another home's stamp. A host with many homes (the
  agent roster on one host) also gives each its own space, `--agent
  hermes-<name>`; see "Own spaces" in `../AGENTS.md`.
- The final `post_llm_call` line is written <500 ms before
  `on_session_finalize` SIGTERMs the daemon; the serializer's final push
  (see **Watch** in `../AGENTS.md`) is what gets it onto the server for a one-shot session.
- Local testing: where sandboxed-agents is installed, `hermes` on PATH is
  its wrapper (drops `HERMES_HOME`, bind-mounts the real `~/.hermes`). Use
  `hermes-achtung-achtung` with `HERMES_HOME=<scratch>` and a copied
  `config.yaml`/`.env`/`auth.json`; `hermes plugins doctor cccp` validates
  discovery/import/registration without a model call.
- sandboxed-agents ships the plugin from `${cccp}/share/cccp/plugin/hermes`:
  bwrap/runsc bind it onto `~/.hermes/plugins/cccp` via `EXTRA_MOUNTS`
  (no writes into the user's real dir), microvm and seatbelt `ln -sfn` it
  into the effective home. Enabling stays manual for a pre-existing
  `config.yaml` (`hermes plugins enable cccp`); the launcher only warns.
  Both paths are guarded on `[ -d ${cccp}/share/cccp/plugin/hermes ]`:
  sandboxed-agents pins `cccp` as a GitHub flake input, so until the lock
  points at a revision that installs `plugin/`, the wrapper has `cccp` on
  PATH but ships nothing. `hermes/tests/sandbox.nix` asserts the same
  implication (package carries the plugin -> sandbox home has it) so a
  stale lock passes and a broken bind fails.
- Backfill: `register()` also starts a daemon thread (5 s delay, 20 s
  budget per run, resumes next launch) that mirrors every past session from
  `state.db` into the same `transcripts/<id>.jsonl`, so the existing daemon
  and allowlist ship history with no Go-side change. It reads through
  Hermes' own `SessionDB(read_only=True)` (`list_sessions_rich`,
  `get_messages(include_compacted=True)`) and falls back to `mode=ro`
  sqlite with the same display filter + compaction dedupe when
  `hermes_state` is not importable (the `python3 __init__.py --backfill`
  CLI); both paths are asserted byte-identical. Never writes to `state.db`.
- Backfilled lines reuse the live `_line()` helper; the only differences are
  deterministic `uuid5(session, message id, tag)` ids and the message's own
  timestamp, which is what makes re-runs and a lost stamp duplicate-free.
  One assistant row with N `tool_calls` becomes N `tool_use`/`tool_result`
  pairs (results matched on `tool_call_id`, first unconsumed row after the
  call), matching the per-call shape `post_tool_call` emits; `reasoning`
  rides on the first line of the row as `thinking`.
- Ownership rule: a transcript that exists but carries none of our
  deterministic uuids was written by the live hooks and is stamped
  `{"skip": "live"}` forever -- appending DB rows to it would duplicate
  turns the hooks already captured with random uuids. Same reason the
  backfill skips ids in the in-process `_sessions` map and any session with
  `ended_at IS NULL` whose newest message is <15 min old (live in another
  process; Hermes never sets `ended_at` on a crash, so age is the signal).
- Every transcript starts with one `session-meta` line for the server's
  session index: `user`, `model`, `agentSource` (live: the `platform`/
  `source` kwarg of `on_session_start`; backfill: `sessions.source`) and
  `parentSessionId` (live kwarg, backfill `sessions.parent_session_id`), so
  a delegated session rolls up under its parent. Live lines add
  `gitBranch`/`gitCommit` from `git -C <cwd>` (2 s timeout, fail-open);
  the backfill omits them because today's checkout says nothing about a
  past session. The backfill's meta line has the deterministic uuid tag
  `meta`, so re-runs stay duplicate-free; transcripts written before it
  existed simply have none, and the indexer treats every field as optional.
- Tool names map to Claude spellings via `TOOL_NAMES` + `_claude_tool()`
  (`terminal`->`Bash`, `read_file`->`Read`, `patch`->`Edit`,
  `search_files`->`Grep`/`Glob` on `target`, `web_extract`->`WebFetch`,
  `delegate_task`->`Agent`, ...), `path` aliased to `file_path`, original
  name kept as `hermes_tool`; live and backfill share the helper. Synthetic
  lines: `usage` from `post_api_request` (its `usage` kwarg is Hermes'
  `CanonicalUsage`: `input_tokens`/`output_tokens`/`cache_read_tokens`/
  `cache_write_tokens`/`reasoning_tokens`; no cost is passed to plugins, so
  live lines omit `costUSD`), `model-change` from the `model` kwarg on the
  first sighting and every switch (`_Session.set_model`), `isMeta` on user
  text matching `_META_TEXT` (leading `<tag>` or `[System`/`[Cron`/...).
  `_line()` takes `message=None` + `extra` for the message-less types.
- `state.db` has no per-call usage (only `sessions` counters and
  `session_model_usage` per model+task), so the backfill emits one
  `model-change` (uuid5 tag `model`, at min(`started_at`, first message))
  and one `usage` per `session_model_usage` row (tag `usage/<model>/<task>`,
  `costUSD` = actual else estimated) at `ended_at`; both are session totals.
  `session_model_usage` is read with `SELECT *` through the ro sqlite
  connection in both API and SQL modes and tolerates a missing table.
  Under `SessionDB` the test DB's `started_at`/`ended_at` are wall-clock,
  so the API-mode tests assert ordering, not exact stamps. Real-data
  2026-09-08 (scratch copy, 6 sessions): API and SQL paths byte-identical,
  1247 lines, 12 usage lines, 6 model-change, 2 isMeta.
- `cccp/backfill.json` (`sessions.<id>.last_id/size`) makes the
  steady-state run one `get_messages(latest=True, limit=1)` per session and
  zero file reads; `cccp/backfill.lock` (flock) serializes concurrent Hermes
  processes. Both live under `<HERMES_HOME>/cccp/` like OpenCode's, outside
  the allowlist. Until 2026-10-28 they were `transcripts/.backfill.json` and
  `.backfill.lock`: the first run of a newer plugin copies the old stamp's
  sessions to the new path under the new lock when the new one does not
  exist yet, so it skips every session the old stamp already covered instead
  of re-reading them all; afterwards the old file is never read again. The
  plugin never deletes the old file: an older `cccp` binary on the same host
  would push its disappearance as a tombstone. Delete it by hand once every
  host runs the new binary, if at all; nothing reads it. `fixtures/hermes-backfill.jsonl` is a real backfilled one-shot
  session (cwd rewritten) and `TestHermesBackfillTranscriptNormalizes` pins
  it; `plugin/hermes/test_backfill.py` covers both read paths.

## Codex

- Codex 0.153.4 writes one append-only rollout per thread at
  `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<id>.jsonl`, so unlike
  Hermes there is no plugin-side transcript: `plugin/codex/` is just
  `hooks.json` (root-level Claude-style hooks; `codex features list` shows
  `hooks` stable+on and `plugin_hooks` *removed*, so a plugin-dir bundle
  installs but never fires). Hooks read one JSON object on stdin with
  `session_id`/`transcript_path`/`cwd`; `SessionStart` fires on the first
  turn (not process start) and `SessionEnd` on quit with its timeout
  clamped to 3 s. The commands are `command -v cccp && exec cccp watch
  hook-start --agent codex; cat >/dev/null`, so no `cccp` = silent no-op.
- Hook trust: codex refuses new/changed hooks until the user trusts them in
  the TUI ("Hooks need review"), persisted in `config.toml` as
  `[hooks.state."<abs hooks.json>:session_start:0:0"] trusted_hash =
  "sha256:..."` (path + content keyed; the hash scheme is not a plain
  sha256 of the command or the JSON object, so it cannot be pre-seeded).
  `codex exec` skips untrusted hooks silently unless run with
  `--dangerously-bypass-hook-trust`.
- Profile allowlist (`profile_codex.go`, honours `$CODEX_HOME`):
  `sessions/`, `archived_sessions/`, `rules/`, `skills/`, `AGENTS.md`,
  `config.toml`, `history.jsonl`, `hooks.json`, `session_index.jsonl`.
  Never `auth.json` (ChatGPT refresh tokens), `*.sqlite*` (thread history,
  logs, memories, state - all WAL churn), `cache/`, `tmp/`, `.tmp/`,
  `plugins/`, `shell_snapshots/`, `attachments/`, `generated_images/`,
  `models_cache.json`. `skills/.system/` is codex-bundled vendor content
  (~60 files) that rides along with `skills/`; trim it if the core ever
  grows exclusions.
- `convert_codex.go` is stateless per line: `response_item` `message`
  (user `input_text` -> string content, `<environment_context>` dropped,
  other `<...>` prompts marked `isMeta`; assistant `output_text` -> `text`),
  `reasoning` -> `thinking` only when a `summary` exists (encrypted-only
  reasoning is dropped), `function_call`/`custom_tool_call` ->
  `tool_use` keyed on `call_id`, `*_output` -> `tool_result`. `is_error`
  comes from the output structure: a `Script failed` header, a
  `Script error:` item, a code-mode exec_command JSON result
  (`{chunk_id,exit_code,output}`, unwrapped to its `output` with a
  `Process exited with code N` prefix on N != 0), the legacy
  `Process exited with code N` text, or `aborted by user`. The
  `Script completed\nWall time...\nOutput:` header is dropped, other
  headers (`running with cell ID`, `failed`) are kept. `event_msg` is
  dropped except `task_complete`, `turn_aborted`, `token_count`,
  `thread_settings_applied`, `thread_goal_updated` and `item_completed`;
  `compacted` -> `system/compact_boundary` (window number + kept-message
  count in `compactMetadata`); `token_usage_record`, `world_state`,
  `task_started`, `user_message`/`agent_message` event_msgs and
  inter-agent metadata are noise. Titles come from the first plain user
  line via the normal `userPromptTitle` path.
- Code-mode `exec` scripts are parsed with a JS-literal-tolerant reader
  (`jsParser`: unquoted keys, single/backtick strings, comments, trailing
  commas, `const x = ...` identifier resolution one level deep; `${}`
  templates fail closed). 70% of real single-call scripts use unquoted
  keys, so a JSON decoder mapped only 4.6k of 20k calls to Bash. A single
  `tools.X(...)` call maps: `exec_command` -> `Bash` (`command`,
  `workdir`, `description` = `justification`), `apply_patch` -> `Edit`
  (old/new split from the `*** Update File` hunks, raw `patch` kept) or
  `Write` (`*** Add File`), `write_stdin` -> `Bash` (`description:
  "stdin -> session N"`), `view_image` -> `Read`, `web__run` ->
  `WebSearch`/`WebFetch`, `clock__curr_time`/`*_goal`/`image_gen__*` keep
  their name with a `description`. Multi-call or unparsable scripts stay
  `exec` with `{code, description: "<comma-joined tools.X names>"}` and
  render through `templates/tools/codex-exec.html`.
- `item_completed` items: `FileChange` -> `Edit`/`Write` pairs per file
  (unified diff split into old/new, `status` failed/declined -> is_error),
  `ImageView` -> `Read` with an `[image]` result, `Extension` ->
  `WebSearch`/`WebFetch` with formatted results, `SubAgentActivity`
  started/completed/interrupted -> `system/subagent` (`agentPath`,
  `agentThreadId`; `interacted` is skipped), review mode enter/exit ->
  `system/review`. Those lines carry `"codexItem":"<Type>"` because
  `FileChange`/`Extension` duplicate the `apply_patch`/`web__run`
  response_item pairs when both exist (item ids `exec-<uuid>` do not
  match `call_id`s, so the converter cannot pair them); the other item
  types duplicate response_items outright and are skipped.
- Collab `function_call`s keep their names (`send_message`, `followup_task`,
  `spawn_agent`, `wait_agent`, `wait`, `list_agents`, `interrupt_agent`,
  `sleep`) and render via `templates/tools/codex-collab.html`; a Fernet
  `gAAAAA...` message is replaced with `[encrypted message, N chars]`
  (32% of the corpus). `request_user_input(_async)` -> `AskUserQuestion`
  with Claude's `questions[].{question,header,options[].label}` shape and
  the `{"answers":{id:{answers:[..]}}}` result flattened to `id: answer`.
  `response_item/agent_message` (inter-agent mail) -> assistant text
  headed `**author -> recipient**`; developer messages -> `isMeta` user
  lines except `<permissions instructions>`, `<skills_instructions>`,
  `<apps_instructions>` and the guardian "Use prior reviews" boilerplate.
- Synthetic lines for pravda: `usage` from `event_msg/token_count`
  `last_token_usage` (present in every rollout and per-response; its sum
  matches the final `total_token_usage` within 1-2%, the gap being 2.5%
  consecutive duplicate `token_count` events; `token_usage_record` exists
  in only half the files and would double count). `input_tokens` is
  `input_tokens - cached_input_tokens` so Claude's input+cache_read
  semantics hold; `model` is empty because `token_count` carries none.
  `model-change` from every `turn_context` (`effort`, falling back to
  `collaboration_mode.settings.reasoning_effort`) and
  `thread_settings_applied` (`reasoning_effort`); `session-meta` only from
  `session_meta` with `model` (`base_instructions.provenance.model`),
  `modelProvider`, `gitCommit`, `gitRepo`, `agentNickname`, `agentPath`.
  `task_complete.error` -> `system/turn_duration` with `error` =
  `codex_error_info` and `errorMessage`; `time_to_first_token_ms` ->
  `timeToFirstTokenMs`.
- `fixtures/codex-rollout.jsonl` is spliced from two real local rollouts
  (paths rewritten, encrypted blobs replaced, long texts truncated) plus
  hand-built lines for every mapping above; `convert_codex_test.go` pins
  the line budget (62 converted lines from 71) so new noise or lost
  content both fail.
- sandboxed-agents copies `${cccp}/share/cccp/plugin/codex/hooks.json`
  into the effective codex home from `sandboxInitLines` (same
  `_CODEX_HOME` resolution as claude's `.config.json`), because the real
  `~/.codex` is bind-mounted over `$SANDBOX_HOME/.codex` and would shadow
  a `configDir` seed. Fresh -> copy; ours but stale -> overwrite (codex
  then re-prompts for trust, which is the right UX); foreign content ->
  warn and leave alone. It also `mkdir -p`s `<home>/cccp`. The user still
  needs `<home>/cccp/config.json` and a one-time trust in the TUI.
- Local testing without touching `~/.codex`: `CODEX_HOME=<scratch>` plus
  the `decent` provider (`-c model_provider=decent -c
  model_providers.decent.env_key=LLM_API_KEY -c model=llm-default`; it
  speaks the Responses API, `wire_api = "chat"` is gone). Drive the TUI in
  a private tmux server (`tmux -S <sock>`) for the trust prompt.

## OpenCode

- Profile `opencode`: `DefaultDir` is `$XDG_DATA_HOME/opencode`
  (`~/.local/share/opencode`), the same rule the plugin and OpenCode's own
  `xdg-basedir` use, so all three agree without a shared config. Allowlist
  is only `transcripts/`: the data root also holds `opencode-stable.db*`
  (sqlite, churns every turn), `log/`, `snapshot/`, `tool-output/`,
  `shell/`, `auth.json` (provider credentials) -- none of it line-syncable,
  some of it secret.
- `plugin/opencode/cccp.js` emits Claude-shaped jsonl directly, so
  `convert_opencode.go` is identity (it drops blank / non-object lines) and
  `fixtures/opencode-run.jsonl` (a real `opencode run` turn on 2.1.4 against
  a mock OpenAI-compatible provider: read tool, then text) plus
  `opencode-synthetic.jsonl` (the node harness: thinking, two tools, an
  error result, a patch, a model change) pin the shape. A change to the
  plugin's output must update the fixtures.
- OpenCode 2.x (since 2026-10, pinned by sandboxed-agents) changed the plugin
  API: a plugin is `export default { id, setup(ctx) }` (or `{ id, effect }`);
  1.x modules (`export const X = async ({ client }) => ({ event })`) fail with
  `PluginModule.LoadError ... Missing key ["default"]` and are skipped while
  OpenCode keeps running. Events come from `for await (e of
  ctx.event.subscribe({ signal }))` as `{id, created, type, data, location?,
  durable?}`. `message.updated` / `message.part.updated` / `session.idle`
  are gone; the plugin builds each assistant step from
  `session.step.started|ended|failed`, `session.text.*`,
  `session.reasoning.*` (per-kind `ordinal`s within a step), and
  `session.tool.input.started` (name) / `.called` (input, start) /
  `.success|.failed` (content or error, end); prompts from
  `session.inbox.enqueued` (payload) + `.delivered` (the user message, id =
  `inboxID`, time = the delivered event's), injected text from
  `session.synthetic` (message id = `evt_` -> `msg_`); a turn ends with
  `session.execution.succeeded|failed|interrupted`. Every event's `created`
  equals the stored message time it projects to (step `data.started` =
  assistant `time.created`, tool called/success = `time.ran`/`completed`,
  step ended = `completed`), which is what makes the live and backfill lines
  byte-identical (only the `ai-title` timestamp differs).
- uuids: 2.x content has no ids, so lines use `<msgID>-text-<n>`,
  `-reasoning-<n>`, `-tool-<callID>` (+ `-result`), `-usage`, `-patch`,
  `-error`, `-model` (msgID = the assistant message, one per step), the user
  message id for prompts. Text timestamps are derived (the previous item's
  end, else the step start) because stored text has no time. A step's last
  item waits for the step's end so it carries `stop_reason`/`usage`.
  Tool names map to Claude spellings (`shell` -> `Bash`, `subagent` ->
  `Agent`, `todo` -> `TodoWrite`, `patch` -> `Edit`, plus the 1.x names);
  `filePath`/`oldString`/`newString` get the snake aliases and `path` gets
  `file_path` for read/write/edit (originals kept).
- Where plugins run: in the server. The 2.x CLI/TUI attach by default to a
  shared background service (`opencode serve --service`, detached, survives
  the command, registry `$XDG_STATE_HOME/opencode/service-stable.json`, fixed
  port per channel -- stable is 52031, override with `{"port": N}` in
  `$XDG_CONFIG_HOME/opencode/service-stable.json`); `--standalone` or
  `{"disabled": true}` there gives each command a private `opencode serve
  --stdio` child, SIGTERMed at exit, which runs plugin cleanups and
  `process.on("exit")` (verified). So with the service `hook-end` fires when
  the service stops; standalone it fires per command as in 1.x. The service
  inherits the environment of whichever command started it.
- Plugins are instantiated per location (directory) on a global bus, and
  reloaded (cleanup + setup) when their file changes: state lives on
  `globalThis` under `Symbol.for("cccp.opencode:<data root>")`, events are
  deduped by id, the exit hook and the backfill run once per process.
- Startup race (seen on every cold service start): plugins load lazily on a
  location's first request, usually the one creating the session, so
  `session.created` and even the first `inbox.enqueued` can be missed. A
  session first seen without `session.created` is caught up: its events are
  queued, `ctx.session.get` gives directory/parent/title, `ctx.session.context`
  the messages so far (written through the same emitters), then the queue
  replays. Same path for a session resumed in a new process (`--continue`,
  the TUI). Messages created (assistant: completed) at or before the
  transcript's last content line are skipped there and in backfill, so a
  1.x-recorded transcript (part-id uuids) is not re-appended under the 2.x
  uuids.
- OpenCode sessions never end except by deletion, so `hook-end` runs on
  exit for every hooked session, not per session. Child sessions (`subagent`
  tool) have `parentID`; they get their own transcript but no hook.
- The first line appended to a transcript is preceded by a `session-meta`
  line (uuid `<id>-meta`, so it is written once per file even across
  processes) carrying `parentSessionId` (the `parentID`), `agentSource`
  (`subagent` for children), `user` (`$USER`, else `os.userInfo()`) and,
  on the live path only, `gitBranch`/`gitCommit` from `git -C <dir>`.
  `version` is `session.created`'s, else `ctx.app.version` (2.x session info
  stores none). OpenCode's generated session title (`session.renamed`)
  becomes an `ai-title` line (uuid `<id>-title-<sha1 prefix>`, one per
  distinct title; the `New session -` placeholder is skipped), which beats
  the first prompt as the title just like Claude's. Both are emitted lazily,
  so a session that never writes a message still leaves no transcript.
- The `run` command stores the prompt wrapped in quotes; the transcript
  keeps opencode's bytes.
- Discovery (2.x): `{plugin,plugins}/*` under each config dir (global
  `$XDG_CONFIG_HOME/opencode`, every `.opencode/`): `.js`/`.ts` files,
  directories (entry `server` or `index`), and symlinks to either. A path in
  the config's `plugin` list must be a directory ("configured plugin path
  must be a directory" otherwise). Load errors go to the server log only
  (`failed to load plugin`); `opencode serve --print-logs` in the
  foreground shows them.
- Local testing: build the binary (`nix build
  path:./projects/sandboxed-agents#opencode`; the wrapper names the store
  binary) and run it with `HOME` and the four `XDG_*` dirs pointed at a
  scratch tree (`HOME` too, so nothing under the real `~/.config/opencode`
  is read), a scratch `opencode.json` with an openai-compatible
  provider pointed at a mock (`"npm": "@ai-sdk/openai-compatible"`, a
  stdlib python SSE server answering `/v1/chat/completions` with scripted
  tool calls is enough), `{"port": N}` in the scratch
  `service-stable.json` so it does not clash with another service, and a
  stub `CCCP_BIN` that logs argv + stdin. Kill the scratch service by the
  pid in its registry before wiping the scratch data dir (a running service
  over a deleted database answers 500).
- sandboxed-agents copies `${cccp}/share/cccp/plugin/opencode/cccp.js`
  into the *host's* `$XDG_CONFIG_HOME/opencode/plugin/` at init (that dir
  is what bwrap binds at `~/.config/opencode`; a bind of a single file
  would leave an empty mount-point file behind in the user's real dir).
  Ownership is the `// @cccp-plugin opencode` first line: ours is refreshed
  when it differs, a foreign file is left alone with a warning. Guarded on
  `[ -f ... ]` because the pinned `cccp` input predates `plugin/`.
- Backfill: the plugin walks history 2 s after load (deferred, fail-open,
  ~20 s budget, 100 ms between calls, resumable via a `pending` cursor) and
  writes past sessions through the same emitters. Listing is
  `ctx.session.list({ limit: 50, cursor })`, global across projects when no
  `directory`/`project` filter is given, newest `time.updated` first; its
  cursor is base64url JSON `{anchor: {id, time, direction}}`, so the stamp
  stores `{id, time}` and rebuilds it. Messages come from
  `ctx.session.context` (the plugin context has no `session.messages`):
  messages after the last compaction only, so compacted sessions backfill
  their post-compaction window. A 1.x stamp's numeric pending cursor is
  ignored (restart from the newest); its `updated` floor still applies.
- The stamp (`cccp/backfill.json`) and lock (`cccp/backfill.lock`) live
  under `<root>/cccp/`, never `transcripts/`: `Walk` ships dotfiles too, so
  a stamp there would sync to every host and make them skip their own pass.
- Live-session skip covers this process's `sessions` map plus the daemon's
  `sessions.json` (a `[]string` of ids); the lock holds a pid, reclaimed
  when dead or older than 10 min. Sessions with `time.updated` equal to the
  stamp are re-listed on purpose, so one boundary session re-fetches per
  launch and dedups. `CCCP_BACKFILL=all|off`, `CCCP_BACKFILL_BUDGET_MS`,
  `CCCP_BACKFILL_DELAY_MS`; harness `node --test
  plugin/opencode/test/backfill.test.mjs` (stub plugin context with a
  scripted event stream, cache-busted imports per test since the module
  reads env at load; `load()` drops the per-root `globalThis` state so each
  load acts as a new process).
- Real 2.1.4 check (2026-10-25, mock provider): runs with a shell
  tool, reasoning, a subagent (child transcript with `parentSessionId`, no
  hook) and a read tool; service mode (hook-start once per top-level
  session, hook-end at service stop) and `--standalone` (hook-start +
  hook-end per command, also with `--continue`); a session run while the
  plugin was absent was backfilled on the next service start; wiping
  `transcripts/` and forcing `CCCP_BACKFILL=all` reproduced every live line
  byte for byte except the `ai-title` timestamps.
- `opencode-synthetic.jsonl` is pinned by a harness test that replays the
  scenario through the live event path; regenerate with
  `CCCP_FIXTURE_WRITE=1 node --test plugin/opencode/test/backfill.test.mjs`
  (the harness strips `CCCP_*` from the env at load, so the flag is read
  before that). Transcripts written before a new line type only gain it via
  `CCCP_BACKFILL=all`, appended at the end (uuid dedup), and never gain
  `isMeta` on already-written user lines.
