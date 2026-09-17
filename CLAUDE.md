# CLAUDE.md - cccp

Product docs: [`README.md`](README.md), [`man/cccp.1`](man/cccp.1).
This file is what's not obvious from reading the code.

## What this is

Cloud code config provider: one Go binary that syncs `~/.claude` across
machines (see README for the product pitch). Sync modes by file type:
last-write-wins for normal files, append-only dedup for `*.jsonl`,
tombstones for deletions. cheka is a push-side secret scanner.

**Out of scope:** telemetry, eviction, PII scrubbing, built-in TLS.

## Layout

```
cmd/cccp/             single binary (serve, push, pull, watch, explore)
internal/config/      4-layer resolver (defaults < file < env < CLI)
internal/agent/       client-side walk / push / pull / host id
internal/api/         server handlers (ingest, blob, current, log, events SSE)
internal/cheka/       secret scanner (push-side only)
internal/store/       SQLite schema + queries
internal/secretary/   watcher + SSE subscriber + serializer goroutines
internal/pravda/      HTMX session viewer - pages, render, embedded assets
nix/                  systemd module + NixOS e2e test
man/                  groff man pages
completions/          bash / zsh / fish
```

Deps: `google/uuid`, `modernc.org/sqlite`, `fsnotify`,
`gorilla/websocket`, `goldmark` + stdlib.
Raw `http.ServeMux`, SQLite in WAL mode behind a small pool
(`SetMaxOpenConns(4)`, so a long page read never stalls ingest; writes
queue on `busy_timeout`), `log/slog` to stderr.

## Store

- `log_paths` is a trigger-maintained summary of `log_lines` (count, last
  time, byte total, first host per path). pravda's session list reads only
  it: any aggregate over `log_lines` itself is a multi-minute scan on a
  30 GB store (measured: 137 s through the blobs and 220 s through a
  covering index, both kernel-bound page reads), so never add one to a
  request path. `Open` backfills the table once when it is empty; on a
  large store that start takes minutes and the service is down meanwhile.
  The backfill scans the table `NOT INDEXED`: the WITHOUT ROWID primary
  key is (path, line_hash), so the scan is already grouped by path and
  sequential, and `LENGTH(contents)` needs no overflow pages. An expression
  index on `LENGTH(contents)` does not help (strace: 27 preads per
  row, the row is fetched anyway), and the single `MIN(first_seen)` makes
  the bare `first_host` come from that row.
  Build it by hand first for zero downtime: aggregate in a read
  transaction, then fold in `first_seen > cursor` and create the trigger
  in one write transaction (`first_seen` is monotonic).
- Session pages and live updates load lines through `SessionLines(path,
  after)`: the first view reads the whole session in display order, later
  ones only `first_seen > cursor` via `log_lines_path_first`. On a 30 GB
  store the largest codex rollout is 960 MB (14 s to read); before this, every view
  and every live append re-read all of it. `first_seen` is monotonic and
  lines are pushed in file order, so cursor order is file order; the old
  reader path (`parsedCache.parse`, FS store) already assumed a stable
  prefix. Converters stay per-line, so they apply to the delta unchanged.
- `path_facts` persists the sidebar facts (title, cwd, ids) per path with
  the line count they were extracted from; facts come from the first 60
  lines, so a row taken at >= 60 lines is final and shorter sessions are
  re-read only when they grow. Without it a restart re-read 16 k session
  heads on the first index load (23 s).
- A store that ran the covering-index attempt (fc5f52a50) still carries
  `log_lines_path_stats`. Nothing reads it; it costs an extra b-tree insert
  per line and ~0.5 GB per 30 GB. Dropping it walks every index page, which
  on a large store is minutes, so do it by hand in a quiet moment, never
  from a migration.

## Build / test

```sh
go build ./cmd/cccp
go test ./...
go vet ./...
nix build .#cccp
nix build .#checks.x86_64-linux.e2e   # NixOS VM; Linux only
```

Config tests use `isolateEnv(t)` - any new env-touching test must too.

## Invariants

**Config:**
- False and unset are not the same. Pointer-per-layer pattern.
- Allowlist is config-file-only. No `--allowlist` or `CCCP_ALLOWLIST`.
- `--port` slices into `--addr` via `withPort()`. Not independent.
- Token allowed in all four layers. Deliberate.
- `DisallowUnknownFields()` on config JSON. Don't loosen.
- Two-pass discovery: compute `$dir` first, then load config file.
- Explicit `--config` missing = error. Implicit missing = fine.

**Sync:**
- `*.jsonl` routes to log pipeline by suffix, not allowlist position.
- Symlinks resolve to target content, shipped under link's path.
- `file_versions` is append-only. Deletion = `content_sha IS NULL`.
- Partial index on `content_sha WHERE NOT NULL`. Don't make it full.
- Log cursor uses `first_seen` (server clock), never `suggested_time`.
- `log_lines` PK is `(path, line_hash)`. Dedup is content-based.
- Ingest is two-phase: a manifest entry whose blob is missing is NOT
  committed - it lands in `need`/`pending`. The client uploads the blobs
  and re-sends the same manifest. `committed`/`skipped` from the second
  response are authoritative: assign, don't accumulate, or unchanged
  paths get counted twice.
- Push never aborts on one bad file. Unreadable blobs are skipped but
  stay in `KnownPushedBlobPaths` (no false tombstone) and keep their
  `BlobCache` entry; unreadable log files keep their prior offset.
  Blobs larger than `MaxPushBlobBytes` (1 GiB, the server's
  `MaxBlobBytes` cap) are skipped the same way, before any read: the
  upload could only ever fail server-side, and an upload failure aborts
  the whole push before `SaveState` -- one oversized file would
  otherwise wedge the host into re-hashing and re-scanning everything
  every cycle while re-shipping the giant body each time. The
  run continues so `SaveState` still executes - aborting would leave the
  cache stale and force re-hashing everything next cycle. Unresolvable
  symlinks (dangling, or target unstattable) follow the same rule: Walk
  returns them as a `skipped` list instead of erroring, and push keeps
  their `KnownPushedBlobPaths` / `BlobCache` / `LogOffsets` entries - a
  broken link is "content unavailable", never a deletion.
- Tombstone application is guarded. Pull removes a tombstoned path only
  when the local bytes hash to a sha the server already knows
  (`PulledSHAs`, rebuilt each pull, or `BlobCache`); a locally modified
  file is kept and re-uploaded by the next push - concurrent edit beats
  delete, silently losing a never-uploaded edit is worse than a
  resurrected file.
- An empty `/current` tar with a non-empty `KnownPulledBlobPaths` skips
  the whole removal pass and keeps the old state lists. A wiped or
  freshly-pointed server must not mass-delete local files, and the
  deletions must not then be pushed back as tombstones fleet-wide.
- Ingest refuses to resurrect: a live manifest entry whose `mtime_ns` is
  <= the latest tombstone's `ingested_at` is skipped, so a host that
  pushes its full tree before pulling the tombstone doesn't undo the
  deletion. Genuine re-creation carries a newer mtime and commits.
- Client-side log lines whose JSON wire encoding is >= 4 MiB are skipped
  with a warning and the offset still advances past them: the server's
  scanner would 400 the whole batch and the file's sync would wedge on
  that line forever. The raw-length check runs BEFORE cheka sees the line,
  in both `scanLogFile` and `pushLogFile`: regex-scanning and redacting a
  4 MB codex rollout line that is about to be dropped anyway pinned a push
  at 100% CPU for nine minutes (2026-09-08).
- `/log/append` requests are chunked at `LogAppendChunkBytes` (16 MiB;
  a package var so tests can shrink it), and `LogOffsets` advances per
  flushed chunk -- `pushLogFile` returns its best offset even on error
  and the caller always takes it. A single unbounded POST meant the
  whole unsynced tail lived in RAM twice (tail + wire body) and a body
  over the proxy's cap could never land, wedging the file at full-size
  allocations per cycle with the offset pinned forever.
- Never bufio.Scanner over local log files - a >4 MiB line silently ends
  the scan mid-file (truncated dedup set = duplicate appends, unscanned
  cheka tail). ReadAll + bytes.Split everywhere the client reads its own
  logs; Scanner stays only on server responses, which are line-capped.
- `pull --full` rewrites each log file to server lines (canonical order)
  plus local-only lines appended after, and resets `LogOffsets[path]` to
  the byte length of the server portion. Unpushed lines survive the
  rewrite and are exactly what the next push sends; a stale offset into
  a rewritten file would push a mid-line fragment that gets deduped
  server-side forever.
- `first_seen` is assigned inside `store.AppendLogLine`, under a mutex,
  strictly monotonic, seeded from `MAX(first_seen)` at Open. Assigning
  it in the handler let a slow insert commit below an already-served
  cursor and the line became permanently invisible to `first_seen > ?`
  incremental pulls.
- `BlobCache` is keyed on `(mtime_ns, size)`. Hit = reuse cached sha and
  skip the cheka scan; zero disk reads for that path.
- `LogOffsets[path]` is the byte position after the last consumed `\n`.
  A partial trailing line (concurrent writer) is held back until it's
  newline-terminated. Offset > file size means truncation: rescan from 0.
- `/current` builds the tar fully in memory so a missing blob is a clean
  5xx, never a truncated stream after headers flushed.
- Pull merges log lines sorted by `(suggested_time, first_seen)`; local
  dedup is by line content hash. Writes are tmp-file + rename.
- `suggested_time` comes from client-side best-effort extraction:
  JSON keys `timestamp`/`ts`/`time`, RFC3339 or numeric epoch upscaled
  to ns by magnitude (`<1e11` s, `<1e14` ms, `<1e17` us).

**Agents:**
- `--agent NAME` (`CCCP_AGENT`, config key `agent`, default `claude`)
  selects a `Profile` from `internal/agent/profiles.go`: the default `--dir`
  and the default allowlist. Profiles register themselves from
  `internal/agent/profile_<name>.go` in `init()`; an unknown name is a
  hard error that lists the registered ones. Two-pass resolution: the
  agent decides where `config.json` is looked for, and a file that sets
  `agent` re-derives the default dir afterwards.
- Every non-claude agent ships under the wire prefix `agent/<name>/`
  (`WirePrefix`). `Walk` applies it to `RelPath`, so push, `state.json`
  keys, tombstones and log offsets all live in wire space with no other
  code aware of it; pull maps wire paths back with `localRel`, which also
  makes claude ignore `agent/**` and every other agent ignore everything
  outside its own prefix. Singular `agent/` on purpose: `~/.claude/agents/`
  is a real Claude Code directory already in the allowlist.
- The secretary watcher is scoped to the allowlist (top-level dirs plus
  single files), not the whole root: a non-claude root is full of sqlite
  WAL churn that would otherwise fire a push every few seconds.
- A non-claude daemon minting its `host_id` copies `~/.claude/cccp/host_id`
  when present, so one machine is one host in pravda whatever the agent.
- Pravda: `internal/pravda/convert.go` wraps the `SessionStore`; a
  `Converter` registered per agent turns each raw line of an
  `agent/<name>/**/*.jsonl` file into zero or more Claude-Code-shaped
  lines before `ParseJSONL`/`Normalize` see them. Converters must be
  stateless per line: `parsedCache` relies on the output being append-only
  when the input is. Agents whose plugin already emits the Claude shape
  register an identity converter so the path is recognised.
  `RegisterSessionFilter(agent, keep)` narrows what the sidebar lists for an
  agent (codex: `sessions/`, `archived_sessions/`), since every
  `agent/<name>/**/*.jsonl` is a log path but not every log is a session.
- Plugins (hook wiring per agent) live in `plugin/<agent>/` and the nix
  package installs them at `$out/share/cccp/plugin/<agent>/` for
  sandboxed-agents to bind in.

**Watch:**
- The serializer reconciles every minute (push, then pull unless push-only),
  so missed watcher/SSE events and an idle host after an outage recover without
  another file write. Failed pushes retain the dirty flag for shutdown flushing.
- Partial log upload failures save acknowledged offsets and then return an
  error; callers must not treat an incomplete upload as success. Each upstream
  upload request has a five-minute deadline, independent of the SSE connection.
- Push and pull are serialized (shared `state.json`). One serializer
  goroutine drains a trigger channel and coalesces pending push/pull.
- Two echo layers: the SSE feed filters events whose host matches the
  subscriber's `X-Host-Id`; anything that slips through is neutralized by
  content-addressing (the re-push computes the same sha, `/ingest`
  reports skipped). One extra round-trip, no "just-wrote" caches.
- The broadcaster never blocks: slow SSE subscribers drop events. Safe
  because pulls are idempotent - the next tick covers it.
- Debounce is trailing-edge: no signal, no fire. The daemon therefore
  queues one push itself at startup: files written before it came up
  (a transcript from a session that ended, a host whose secretary was
  only just enabled) produce no fsnotify event and would otherwise sit
  unsynced until the next unrelated write (seen 2026-09-08).
- The serializer never pulls over unpushed local changes: the watcher
  sets a `localDirty` flag immediately (before the debounce fires), a
  pull trigger arriving while dirty is promoted to push-then-pull, and
  while the last push failed (cheka block, oversized blob, network)
  pulls are skipped entirely until a push succeeds. Otherwise a remote
  SSE event lands mid-debounce and the pull reverts the just-saved edit
  with nothing left to recover it from.
- Session lifecycle owns daemon. Zero sessions = SIGTERM. On SIGTERM the
  serializer waits `finalPushGrace` for fsnotify to deliver, then runs one
  last push if `localDirty` is set or no push has succeeded yet: a one-shot
  session's last line is written milliseconds before the end hook fires,
  and the debouncer would otherwise take it to the grave. `hook-end` then
  waits up to 5 s for the pidfile to go, so a sandbox torn down right after
  the agent exits still gets the flush.
- `~/.claude/cccp` excluded from watcher (else state.json writes feed back).
- Watch defaults `--cheka-redact` to true unless a layer explicitly set
  it: a daemon that silently stops syncing is worse than redacted lines.
- Daemon files under `<root>/cccp`: `sessions.json` (+ `sessions.lock`
  flock), `secretary.pid`, `secretary.daemon.lock` (held for daemon
  lifetime), `secretary.log`.
- Liveness is tri-state via signal 0: live / gone / EPERM = "probably
  running, can't tell" (sandbox or other uid).

**Explore:**
- Two backends (`FSStore`, `SQLiteStore`), one `Store` interface.
- All HTML server-rendered. No build step. `//go:embed public/*`.
- WebSocket at `/ws/live/{path}` for live updates.
- `parsedCache` exploits append-only JSONL: only lines past the cached
  count get `json.Unmarshal`; a shrunken file clears the cache. Cache
  validity is a per-path unix-nano version stamped on every log event.
- Titles are the first user message and never change - cached forever in
  `titles`. Sidebar list is cached whole, invalidated on any change.
- Session list is server-paginated (50 per page, `?p=`). Pager links always
  carry an explicit `p`: on session pages a missing `p` means "auto-locate
  the page containing the current session", so a p-less prev link would
  bounce straight back. Search (`?q=`) filters the full list before
  pagination; the sidebar/welcome inputs are GET forms to `/` (Enter =
  server-side search across everything, JS keeps live-filtering the
  rendered page).
- Live updates diff on entry fingerprints `(HasResult, len(SubAgent))` -
  much cheaper than diffing HTML. Changed entries go out as
  `hx-swap-oob` fragments keyed on `id="e-{Index}"`; no cached state or
  a shrink forces a full timeline replace.
- `Entry.Index` is 1-based; 0 is the template's "no inspector link"
  sentinel (sub-agent entries keep 0).
- `isHTMX` treats `HX-Boosted` as a full-page request: htmx swaps the
  whole `<body>` for boosted navs, so returning a fragment would wipe
  the sidebar.
- CORS middleware allows credentials only for an explicit allowlist:
  same-origin and localhost/127.0.0.1/::1 dev origins. Any other Origin
  gets no ACAO header. OPTIONS short-circuits before any auth. WS
  upgrades are same-origin only (empty Origin = non-browser, allowed).
- Sub-agent transcripts live under `<dir>/<session>/subagents/` -- flat
  `agent-<id>.jsonl`, or nested `workflows/<wf>/agent-<id>.jsonl` and
  `workflows/<wf>/journal.jsonl` since Workflow runs -- and `subagentRe`
  claims the whole subtree, so every such file lists under its parent
  session (label `wf › id`) instead of surfacing as a top-level session
  with a tool_result JSON blob for a title. Sidechain events are attributed
  to their Task/Agent tool_use by walking `parentUuid` to the first
  non-sidechain ancestor.
- `facts.go`: every listed session gets `sessionFacts` from its first
  `headLines` (60) lines -- title, cwd, model, version, git branch, session
  id, parent session id, agent source -- cached in `titles` in place of the
  old title string. The sidebar folder is the prettified cwd
  (`~alice/…/a/b`) for every agent, falling back to the Claude project slug;
  each row carries an agent badge and, on the SQLite store, the host name of
  the first line (`LogPathInfo.Host`). Codex child threads (`session_meta`
  with `parent_thread_id`, converted to a `session-meta` line) nest under
  their parent like sub-agents, labelled by `thread_source`
  (`guardian_review`). The header shows the same facts plus a parent link.
- Hidden kinds are dropped server-side (`timelineData.Visible`, keyed like
  the filter cookie: `kind` or `attachment:<subtype>`), not just hidden by
  the `hide-*` body class: one Claude session carried 1,899 hidden
  `total_tokens_reminder` articles and weighed 9 MB. The timeline funcs
  `markdown`, `resultText` and `json` clip at 32 KiB with a pointer to the
  inspector, which uses the `*Full` variants; a guardian-review codex file
  with 3,339 user messages of 73 KB each rendered 247 MB before the cap.
  The timeline is also windowed: `?limit=N` (default `timelineWindow` = 400)
  renders only the last N visible entries with an "earlier / show all"
  notice on top; `?limit=0` is everything. `Entry.Index` stays global, so
  `?event=` inspector links and live `e-N` swaps are unaffected. Live
  full-timeline replaces pass no hidden set and no limit (the body class
  still hides them), so they are the one path that renders everything.
- `env.go`: the session page builds a `sessionEnv` from the full parse --
  model spans (transitions of `message.model` + top-level `effort`, and the
  synthetic `model-change` lines converters emit), per-model token totals
  (`message.usage` on Claude assistant lines, synthetic `usage` lines from
  the codex/opencode/hermes plugins, and Claude's `cost-state` when present,
  which is authoritative), runtime facts from `attachment:environment`,
  skills listed/invoked, MCP servers (from `mcp_instructions_delta` and
  `mcp__<server>__` tool names), hooks fired, mode/permission transitions
  and tool call counts. It renders as the collapsible Environment panel
  under the header; the raw attachments it summarises never appear in the
  timeline. `usage` and `session-meta` lines are dropped in `entriesFor`.
- Filter presets (`presets` in env.go): Conversation hides everything but
  user/assistant/tools (the `attachment` key hides every subtype at once),
  Standard is `defaultHidden`, Everything is empty. `POST /settings/preset`
  writes the same cookie the checkboxes use, so `presetOf` recognises the
  three states and highlights the button.
- Sidebar grouping is a cookie (`cw_group`: `none` | `project` | `host`,
  default `project`) toggled by the chips under the search; groups are
  built per page from the paginated flat list (`groupItems`), ordered by
  first appearance so the newest activity stays on top, with sticky
  headers. The folder fallback for sessions without a cwd turns the Claude
  project slug back into a path so `-srv-repo` and `/srv/repo` land in one
  group.
- Codex child threads get a `child-session` marker entry in the parent
  timeline at the child's first timestamp (`childMarkers` + `mergeMarkers`,
  markers have Index 0 so `e-N` ids are untouched); the marker, like
  workflow `result` entries, lazy-loads the child transcript inline from
  `/subagent/<path>` via HTMX and links to the full page.
- `?agent=<name>` filters the list; chips with per-agent counts render
  above the list whenever more than one agent is present. Search also
  matches host and agent.
- Titles skip meta lines, `# AGENTS.md` preambles, and user lines whose
  content is only tool_result blocks; a session with no real prompt titles
  from the first assistant text, then the model.
- Assistant entries are labelled by agent (`Entry.Agent`, set per session
  from the path), never a hardcoded "Claude". `tool-generic` previews the
  first meaningful input field (`command`, `query`, `path`, ...) so hermes
  `terminal` and codex `exec` read at a glance.
- `Entry.TemplateNames()` returns candidates most-specific first and
  `renderEntryContent` takes the first that exists. Attachments fall through
  `attachment-<subtype>` -> `event-attachment` -> `event-unknown`; systems
  through `system-<subtype>` -> `event-system`. It used to return a single
  name, so every attachment without its own `attachment-<subtype>` define
  landed in the raw-JSON "no renderer" box and the whole subtype dispatch
  inside `event-attachment` was dead code. Add a subtype branch there, not a
  new file, unless the renderer is big enough to want its own define.
- Claude Code keeps adding event types. `fixtures/new-kinds.jsonl` carries one
  synthetic line per type seen in the wild and `newkinds_test.go` asserts each
  reaches a real renderer -- add a line there when a new type appears, and it
  will fail until a branch exists. To find them, group a day of live logs by
  `type` / `attachment.type` / `subtype`.
- Session-level types carry no timeline value and are dropped in
  `entriesFor`: `bridge-session`, `ai-title`, `custom-title`, `agent-name`,
  `summary`, `file-history-snapshot`, `file-history-delta`, `last-prompt`.
  Titles still read them -- `custom-title` beats `ai-title`/`summary`, which
  beat the first user prompt.
- `mode` and `permission-mode` repeat on nearly every turn; only transitions
  become entries (the last value is tracked on `normalizeCtx`). `pr-link`
  repeats per turn too and dedupes on URL.
- `system` subtypes are lifted into typed fields rather than dumped as JSON:
  `local_command` becomes a `command` entry, `compact_boundary` carries the
  token delta, `model_refusal_no_fallback` the refusal category/explanation.
- `<bash-input>` / `<bash-stdout>` + `<bash-stderr>` (the `!` passthrough) are
  their own kinds, and stderr renders in warning-orange beside stdout.
- MCP tools (`mcp__server__tool`) label as the bare tool name and get a plug
  icon; the full name stays in the inspector.

**Responsive:**
- SAE: `sae_settings.go` supplies the shared settings/map payload. It includes
  every eligible message and all stored activations; browser display thresholds
  must not affect coverage. Missing imported records are distinct from low or
  absent activations. `sae.js` owns feature selection, colors, and display modes;
  dictionary defaults use localStorage and conversation overrides use
  sessionStorage, both scoped by encoder. `/sae/session/{path}` refreshes the map
  after live updates without broadcasting one viewer's encoder to other viewers.
  SAE and vote decoration operate on a request-local copy of cached entries.
  The index reports import availability, not external training progress.
- Three breakpoints. Above 1024px the sidebar is 300px. At <=1024px it narrows
  to 240px, the event label column drops 8rem -> 6rem, and the pager wraps
  (it clipped otherwise). At <=768px the sidebar becomes an off-canvas drawer.
- The drawer is a CSS-only `:has(#nav-toggle:checked)` pattern -- the checkbox
  and scrim live in `base.html` outside every htmx swap region, so a boosted
  nav does not reset it. `pravda.js` only closes it on session click.
- The header has a fixed 3.75rem height, so `.session-sub` is clamped to one
  ellipsized line on phones; wrapping pushed the title out of view.
- The tool status badge is `position:absolute; right:-24px`, which hangs off
  the full-width operation row on phones -- it goes back to inline flow there.

**Server/auth:**
- API endpoints accept bearer token, falling back to the `cccp_token`
  cookie. Pravda UI sits behind `cookieAuth`: OPTIONS passes through
  pre-auth (CORS must answer), programmatic callers (Authorization or
  HX-Request set) get 401, humans get 303 to `/login`.
- All-dup `/log/append` requests log at DEBUG - a well-behaved client
  tracks per-file offsets and never re-sends old lines.

**Cheka:**
- Scans on push only, client-side only. No server component. Pull is unscanned.
- Findings logged to stderr, grouped by (path, kind). Push fails if
  secrets found (`--cheka-allow` to adjust).
- Default: zero tolerance. Pass `-1` to disable.
- Pattern order matters: precise service patterns run before generic
  ones, and the first match claims its span - a JWT inside a Bearer
  header, or a key_value hit inside an already-claimed service span,
  yields one finding with the most specific kind.
- Generic patterns (connection_string, bearer, key_value, hex_token)
  denoise their first capture group: known examples, placeholder
  markers (also checked against the surrounding match, e.g. example.com
  hosts), template/format literals, env-var references, dictionary
  words, identifier shapes, Shannon entropy < 2.8.
- Candidate values are truncated at JSON escape sequences (`\n`, `\t`,
  ...) - regexes over quoted jsonl strings run past two-byte escapes.
- hex_token fires only above 64 hex chars (SHA-256 length is fine) and
  is entropy-filtered so binary dumps in session logs don't trigger.
- Redaction can't apply to blobs: it would change the content hash. Log
  lines are redacted in-flight; overlaps resolve left-to-right, longest
  match first at equal starts.
- Anthropic keys contain hyphens in the body; the generic `sk-` rule
  misses them, hence the dedicated pattern (regression-tested).

**Config (extra):**
- An `--addr`/`CCCP_ADDR` value carrying an `http(s)://` scheme is
  treated as `--url` (warn + addr resets to `:12110`) - the user almost
  certainly meant the server URL.

## Style

Pragmatic Go. Three similar lines > premature helper. No emojis, no
verbose docstrings. Default to no comments (run
a comment stripper such as github.com/eordano/strip-comments before committing; it strips
`//` lines including `//go:` directives - restore `//go:embed` in
`internal/store/store.go`, `internal/pravda/server.go`, and
`internal/pravda/render.go` if it eats them). The *why* lives in this
file and the man page, not in the code.

## Agents

### Hermes

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
- Profile allowlist: `transcripts/`, `memories/`, `SOUL.md`, `config.yaml`.
  `skills/` is deliberately out: Hermes seeds it with ~400 bundled files on
  first run, so it is vendor content, not user state. Never `state.db*`,
  `.env`, `auth.json`, `*_cache.json`.
- The final `post_llm_call` line is written <500 ms before
  `on_session_finalize` SIGTERMs the daemon; the serializer's final push
  (see **Watch**) is what gets it onto the server for a one-shot session.
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
- `transcripts/.backfill.json` (`sessions.<id>.last_id/size`) makes the
  steady-state run one `get_messages(latest=True, limit=1)` per session and
  zero file reads; `.backfill.lock` (flock) serializes concurrent Hermes
  processes. `fixtures/hermes-backfill.jsonl` is a real backfilled one-shot
  session (cwd rewritten) and `TestHermesBackfillTranscriptNormalizes` pins
  it; `plugin/hermes/test_backfill.py` covers both read paths.

### Codex

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

### OpenCode

- Profile `opencode`: `DefaultDir` is `$XDG_DATA_HOME/opencode`
  (`~/.local/share/opencode`), the same rule the plugin and OpenCode's own
  `xdg-basedir` use, so all three agree without a shared config. Allowlist
  is only `transcripts/`: the data root also holds `opencode-stable.db*`
  (sqlite, churns every turn), `log/`, `snapshot/`, `tool-output/`,
  `auth.json` (provider credentials) -- none of it line-syncable, some of
  it secret.
- `plugin/opencode/cccp.js` emits Claude-shaped jsonl directly, so
  `convert_opencode.go` is identity (it drops blank / non-object lines) and
  `fixtures/opencode-run.jsonl` (a real `opencode run` turn) plus
  `opencode-synthetic.jsonl` (the node harness: thinking, two tools, an
  error result) pin the shape. A change to the plugin's output must update
  the fixtures. One line per finished *part*, uuid = OpenCode part id
  (`<partID>-result` for the tool_result line); `parentUuid` is just the
  previous line. Tool names are mapped to Claude spellings and
  `filePath`/`oldString`/`newString` get `file_path`/`old_string`/
  `new_string` aliases (originals kept) so the tool templates find them.
- OpenCode does not await plugin `event` hooks (`void hook.event(...)`) and
  `opencode run` calls `process.exit()` right after the session goes idle,
  so every write is `appendFileSync` before the first `await`; ordering
  comes from the message's part list, flushing the finished prefix on each
  update and everything on completion/idle. Effect finalizers (`dispose`)
  are not reliable on that exit path either: `process.once("exit")` runs
  the same shutdown (flush + `hook-end` via `spawnSync`).
- OpenCode sessions never end except by deletion, so `hook-end` runs on
  `dispose`/exit for every hooked session, not per session. Child sessions
  (`task` tool) have `parentID`; they get their own transcript but no hook.
- The `run` command stores the prompt wrapped in quotes in its user text
  part; the transcript keeps opencode's bytes.
- Discovery: OpenCode globs `{plugin,plugins}/*.{ts,js}` under the global
  config dir and every `.opencode/` with `nodir` and no `follow`, so a
  *symlinked* plugin file is silently skipped -- copy it. Plugin load
  errors publish a `session.error` without a sessionID, which `run`
  discards, so a broken plugin is invisible there; `--log-level DEBUG
  --print-logs` is the only signal.
- Local testing: where sandboxed-agents is installed, `opencode` on PATH
  is its bwrap wrapper (pid namespace, PATH rewritten, `cccp` resolves to the
  system one). Use the unwrapped store binary the wrapper names
  (`rg -o '/nix/store/[^"]*opencode-[0-9.]*/bin/opencode' $(readlink -f
  $(command -v opencode))`) with `XDG_CONFIG_HOME`/`XDG_DATA_HOME`/
  `XDG_STATE_HOME` pointed at scratch dirs and a scratch `opencode.json`
  carrying only the keyless `decent` provider (`https://llm.decent.dev/v1`,
  `-m decent/llm-default`). OpenCode still reads the real
  `~/.config/opencode/*.json` read-only regardless of `XDG_CONFIG_HOME`.
- sandboxed-agents copies `${cccp}/share/cccp/plugin/opencode/cccp.js`
  into the *host's* `$XDG_CONFIG_HOME/opencode/plugin/` at init (that dir
  is what bwrap binds at `~/.config/opencode`; a bind of a single file
  would leave an empty mount-point file behind in the user's real dir).
  Ownership is the `// @cccp-plugin opencode` first line: ours is refreshed
  when it differs, a foreign file is left alone with a warning. Guarded on
  `[ -f ... ]` because the pinned `cccp` input predates `plugin/`.
- Backfill: the plugin walks history 2 s after load (deferred, fail-open,
  ~20 s budget, 100 ms between SDK calls, resumable via a `pending` cursor)
  and writes past sessions through the same emit helpers, so the existing
  `transcripts/` allowlist ships them. `session.list` is project-scoped
  (`listByProject` on the instance's project, plus a `directory` filter
  unless `scope=project`); the only cross-project listing is
  `GET /experimental/session`, unwrapped by the SDK, reached as
  `client.session.list({ url: "/experimental/session", query: { directory:
  "" } })` -- the SDK interceptor injects `directory=<instance dir>` only when
  the query lacks it, and that route treats a non-empty `directory` as a
  filter. An empty one resolves the instance from `process.cwd()`, which
  TUI/run/attach `chdir` to. Falls back to `scope: "project"` if the route
  is gone. `session.messages` is routed by the workspace middleware to an
  instance for the *session's* directory (`directory: session?.directory ||
  defaultDirectory`), so each distinct historical directory bootstraps an
  in-process instance once (config + that project's `.opencode/` plugins;
  the module cache and `backfillStarted` keep our plugin from running a
  second pass). `opencode serve` bootstraps nothing until its first request,
  so a `serve`-driven pass needs one `curl` to any endpoint.
- The stamp (`cccp/backfill.json`) and lock (`cccp/backfill.lock`) live
  under `<root>/cccp/`, never `transcripts/`: `Walk` ships dotfiles too, so
  a stamp there would sync to every host and make them skip their own pass.
- Live-session skip covers this process's `sessions` map plus the daemon's
  `sessions.json` (a `[]string` of ids); the lock holds a pid, reclaimed
  when dead or older than 10 min. Sessions with `time.updated` equal to the
  stamp are re-listed on purpose (`gte`), so one boundary session re-fetches
  per launch and dedups.
- History lines equal live lines byte for byte except the final assistant
  line of each message, which carries `stop_reason`/`usage` (the resumed-
  session backfill did this already; live writes the last part before the
  message completes). `CCCP_BACKFILL=all|off`, `CCCP_BACKFILL_BUDGET_MS`,
  `CCCP_BACKFILL_DELAY_MS`; harness `node --test
  plugin/opencode/test/backfill.test.mjs` (stub client, cache-busted imports
  per test since the module reads env at load).
- Real-data check 2026-09-08 (scratch copy of the DB, `opencode serve`):
  20 sessions across 4 projects, 11 transcripts (9 empty "New session" rows
  have no messages), 652 lines, 17.8 s -- `session.messages` in-process is
  ~0.9 s per session, so a first pass over a big history spans launches.
- Synthetic lines (2026-09-08): `usage` per `step-finish` part (uuid = part
  id, `output_tokens` = output + reasoning so the buckets sum to OpenCode's
  total, `costUSD` from the part), `model-change` (uuid `<msgID>-model`) on
  `providerID/modelID` transitions, `system`/`subtype: patch` per `patch`
  part (file list + snapshot `hash`; OpenCode stores no diff in the part).
  Both `step-finish` and `patch` lack a timestamp, so they are written after
  the content once the message completes, with `time.completed` -- that is
  what keeps live and backfill bytes equal. User messages whose first text
  part is `synthetic` or starts with `<tag>` (Buzz `<base>`/`<context>`) get
  `isMeta: true`. `task` now maps to `Agent` (there is a `tool-Agent`
  template, no `tool-Task`). Still skipped: `step-start`/`snapshot` (tree
  hash only), `agent`, `retry`, `compaction` (marker; the next message has
  the summary), `subtask` (child session owns it).
- `opencode-synthetic.jsonl` is pinned by a harness test that replays the
  scenario through the live event path; regenerate with
  `CCCP_FIXTURE_WRITE=1 node --test plugin/opencode/test/backfill.test.mjs`
  (the harness strips `CCCP_*` from the env at load, so the flag is read
  before that). Transcripts written before the new line types only gain
  them via `CCCP_BACKFILL=all`, appended at the end (uuid dedup), and never
  gain `isMeta` on already-written user lines.
  Second launch: 1 boundary session re-listed, 0 lines, 135 ms. Pushing
  that history with plain `cccp push` was blocked by cheka (3 findings in 2
  transcripts); the daemon's default `--cheka-redact` ships them redacted.
  The scratch servers are Bun re-exec'd through `ld-linux-x86-64`, so
  `pkill -f opencode` misses them -- kill by `ss -ltnp` pid.
