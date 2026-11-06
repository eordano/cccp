# AGENTS.md - cccp

Product docs: [`README.md`](README.md), [`man/cccp.1`](man/cccp.1).
This file is what's not obvious from reading the code.

## What this is

Cloud code config provider: one Go binary that syncs `~/.claude` across
machines. Sync modes by file type: append-only dedup for `*.jsonl`,
tombstones for deletions, and a review gate for everything else (agent
state is untrusted input: privileged files never fan out, knowledge files
only once approved). cheka is a push-side secret scanner.

**Out of scope:** telemetry, eviction, PII scrubbing, built-in TLS.

## Layout

```
cmd/cccp/             single binary (serve, push, pull, watch, explore)
internal/config/      4-layer resolver (defaults < file < env < CLI)
internal/agent/       client-side walk / push / pull / host id
internal/api/         server handlers (ingest, blob, current, log, events SSE)
internal/cheka/       secret scanner (push-side only)
internal/syncclass/   per-agent path classes: privileged / knowledge / transcript
internal/store/       SQLite schema + queries
internal/secretary/   watcher + SSE subscriber + serializer goroutines
internal/pravda/      HTMX session viewer - pages, render, embedded assets
nix/                  systemd module + NixOS e2e test
man/                  groff man pages
completions/          bash / zsh / fish
```

Raw `http.ServeMux`, `log/slog` to stderr, SQLite in WAL mode behind a
small pool (`SetMaxOpenConns(4)`, so a long page read never stalls ingest;
writes queue on `busy_timeout`).

## Store

- `log_paths` is a trigger-maintained summary of `log_lines` (count, last
  time, byte total, first host per path); pravda's session list reads only
  it. Never add an aggregate over `log_lines` itself to a request path: on
  a 30 GB store it is a multi-minute scan (measured 137 s through the blobs,
  220 s through a covering index, both kernel-bound page reads). `Open`
  backfills the table once when empty; on a large store the service is down
  for the minutes that takes. The backfill scans the table `NOT INDEXED`:
  the WITHOUT ROWID primary key is (path, line_hash), so the scan is already
  grouped by path and sequential, and `LENGTH(contents)` needs no overflow
  pages. An expression index on `LENGTH(contents)` does not help (strace: 27
  preads per row, the row is fetched anyway), and the single
  `MIN(first_seen)` makes the bare `first_host` come from that row.
  For zero downtime build it by hand first: aggregate in a read transaction,
  then fold in `first_seen > cursor` and create the trigger in one write
  transaction (`first_seen` is monotonic).
- Session pages and live updates load lines through `SessionLines(path,
  after)`: the first view reads the whole session in display order, later
  ones only `first_seen > cursor` via `log_lines_path_first`. The largest
  codex rollout on a 30 GB store is 960 MB (14 s to read); before this every
  view and live append re-read all of it. `first_seen` is monotonic and
  lines are pushed in file order, so cursor order is file order (the old
  `parsedCache.parse` / FS store path already assumed a stable prefix).
  Converters stay per-line, so they apply to the delta unchanged.
- `path_facts` persists the sidebar facts (title, cwd, ids) per path with
  the line count they came from. Facts come from the first 60 lines, so a
  row taken at >= 60 lines is final and shorter sessions are re-read only
  when they grow. Without it a restart re-read 16 k session heads on the
  first index load (23 s).
- A store that ran the covering-index attempt (fc5f52a50) still carries
  `log_lines_path_stats`: nothing reads it, and it costs an extra b-tree
  insert per line and ~0.5 GB per 30 GB. Dropping it walks every index page
  (minutes on a large store), so do it by hand in a quiet moment, never from
  a migration.
- `session_index` (one row per session log), `session_writes` (synced files
  a session's Write/Edit/patch tool calls touched, as wire paths) and
  `session_index_meta` are derived data, folded by `pravda/indexer.go` from
  `log_lines` and never written by ingest: the raw log stays the only
  truth and sync semantics are untouched. A rebuild
  generation snapshots `MAX(first_seen)` as its head, walks `log_paths` in
  path order (resumable via `rebuild_path`), reading each path's lines in
  256-line chunks through `log_lines_path_first`; afterwards the follower
  reads `first_seen > cursor` in 2000-line / 64 MiB batches. The fold keeps
  per-row state (JSON in `state`) and is insensitive to line order, so the
  follower's arrival-order batches and the rebuild's per-path walk produce
  identical rows (checked on 240 k real lines). Every write is a CAS on the
  row's `cursor` (and the follower's on the meta cursor), so a `cccp index
  rebuild` next to serve's follower never folds a line twice; a conflict
  just reloads. The rebuild's cost is reading every line once: on the
  sample 240 k lines / 460 MB took 20-40 s, dominated by overflow-page
  preads of large codex lines, so a 30 GB store is a background job of
  tens of minutes, not a request path. Bump `IndexVersion` whenever the
  fold derives something new; serve then rebuilds in the background.
- Claude splits one assistant message into one line per content block,
  each repeating `message.usage`: usage is counted once per `message.id`
  (index and Environment panel). `cost-state` is per process, carries no
  timestamp (newest by `first_seen` = file order wins) and already
  includes the sub-agents, which never carry one, so a rollup under a
  reporting Claude root skips its sub-agents' tokens and cost. Cost is
  only what the agent reported (Claude `cost-state`, Hermes backfill
  `costUSD`); there is no price table.
- Parent links: Claude sub-agents by path (`subagentRe`); codex child
  threads, Hermes delegations and OpenCode subtasks by `parentSessionId`,
  resolved to a path after each batch (`resolveParents`) against `id` or
  `session_id` of the same agent. A parent may itself be a child (codex
  guardian reviews of sub-agent threads), and the rollup CTE stops at depth
  16. Claude sub-agents share their root's `sessionId`, so the index `id`
  is `<root>/<subagent tail>` for them and `/sessions/{key}` prefers roots.

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
- Token allowed in all four layers. Deliberate. `token_file` is read only
  when no layer set a token.
- `DisallowUnknownFields()` on config JSON. Don't loosen.
- Two-pass discovery: compute `$dir` first, then load config file.
- Explicit `--config` missing = error. Implicit missing = fine.
- An `--addr`/`CCCP_ADDR` value with an `http(s)://` scheme is treated as
  `--url` (warn + addr resets to `:12110`): the user meant the server URL.

**Sync:**
- `*.jsonl` routes to log pipeline by suffix, not allowlist position.
- Symlinks resolve to target content, shipped under link's path.
- `file_versions` is append-only. Deletion = `content_sha IS NULL`.
- Partial index on `content_sha WHERE NOT NULL`. Don't make it full.
- Log cursor uses `first_seen` (server clock), never `suggested_time`.
- `log_lines` PK is `(path, line_hash)`. Dedup is content-based.
- Ingest is two-phase: a manifest entry whose blob is missing is NOT
  committed - it lands in `need`/`pending`; the client uploads the blobs and
  re-sends the same manifest. `committed`/`skipped` from the second response
  are authoritative: assign, don't accumulate, or unchanged paths are
  counted twice.
- Push never aborts on one bad file. Unreadable blobs are skipped but stay
  in `KnownPushedBlobPaths` (no false tombstone) and keep their `BlobCache`
  entry; unreadable log files keep their prior offset. Blobs over
  `MaxPushBlobBytes` (1 GiB, the server's `MaxBlobBytes` cap) are skipped
  the same way, before any read: the upload could only fail server-side, an
  upload failure aborts the whole push before `SaveState`, and one oversized
  file would wedge the host into re-hashing and re-scanning everything every
  cycle while re-shipping the giant body. The run continues so `SaveState`
  still executes - aborting would leave the cache stale and force
  re-hashing everything next cycle. Unresolvable symlinks (dangling, or
  target unstattable) follow the same rule: Walk returns them as a `skipped`
  list instead of erroring, and push keeps their `KnownPushedBlobPaths` /
  `BlobCache` / `LogOffsets` entries - a broken link is "content
  unavailable", never a deletion.
- Tombstone application is guarded. Pull removes a tombstoned path only when
  the local bytes hash to a sha the server already knows (`PulledSHAs`,
  rebuilt each pull, or `BlobCache`); a locally modified file is kept and
  re-uploaded by the next push. Concurrent edit beats delete: silently
  losing a never-uploaded edit is worse than a resurrected file.
- An empty `/current` tar with a non-empty `KnownPulledBlobPaths` skips the
  whole removal pass and keeps the old state lists. A wiped or
  freshly-pointed server must not mass-delete local files, and those
  deletions must not then be pushed back as tombstones fleet-wide.
- Ingest refuses to resurrect: a live manifest entry whose `mtime_ns` is <=
  the latest tombstone's `ingested_at` is skipped, so a host that pushes its
  full tree before pulling the tombstone doesn't undo the deletion. Genuine
  re-creation carries a newer mtime and commits.
- Ingest skips a re-send: a live entry whose sha equals the newest version
  this host sent for the path (looking back `resendWindow` = 64 versions) is
  not a version, even when another host's version is current. Every push
  sends its full manifest, and ingest used to compare an entry only with the
  newest version from any host, so homes whose copies of one path differed
  re-committed theirs on every reconcile: on 2026-10-28 production held 66 k
  versions of `agent/hermes/memories/MEMORY.md` that were three unchanged
  files (three hosts) alternating once a minute, each a pending
  review item, and `config.yaml` / `transcripts/.backfill.json` the same.
  Now the last change wins, not the last push. An edit, or a revert to an
  older content, differs from the host's own newest version and commits; a
  tombstone as the current version skips this rule and leaves re-creation to
  the resurrection guard. Gap: homes that share a host_id (a non-claude
  daemon copies `~/.claude/cccp/host_id`) and differ on one path still
  alternate. Versions already written stay.
- Client-side log lines whose JSON wire encoding is >= 4 MiB are skipped
  with a warning and the offset still advances past them: the server's
  scanner would 400 the whole batch and the file's sync would wedge on that
  line forever. The raw-length check runs BEFORE cheka sees the line, in
  both `scanLogFile` and `pushLogFile`: regex-scanning and redacting a 4 MB
  codex rollout line about to be dropped anyway pinned a push at 100% CPU
  for nine minutes (2026-09-08).
- `/log/append` requests are chunked at `LogAppendChunkBytes` (16 MiB; a
  package var so tests can shrink it), and `LogOffsets` advances per flushed
  chunk -- `pushLogFile` returns its best offset even on error and the
  caller always takes it. A single unbounded POST held the whole unsynced
  tail in RAM twice (tail + wire body), and a body over the proxy's cap
  could never land, wedging the file at full-size allocations per cycle with
  the offset pinned forever.
- Never bufio.Scanner over local log files - a >4 MiB line silently ends the
  scan mid-file (truncated dedup set = duplicate appends, unscanned cheka
  tail). ReadAll + bytes.Split everywhere the client reads its own logs;
  Scanner stays only on server responses, which are line-capped.
- `pull --full` rewrites each log file to server lines (canonical order)
  plus local-only lines appended after, and resets `LogOffsets[path]` to the
  byte length of the server portion. Unpushed lines survive the rewrite and
  are exactly what the next push sends; a stale offset into a rewritten file
  would push a mid-line fragment that gets deduped server-side forever.
- `first_seen` is assigned inside `store.AppendLogLine`, under a mutex,
  strictly monotonic, seeded from `MAX(first_seen)` at Open. Assigning it in
  the handler let a slow insert commit below an already-served cursor, and
  the line became permanently invisible to `first_seen > ?` incremental
  pulls.
- `BlobCache` is keyed on `(mtime_ns, size)`. Hit = reuse cached sha and
  skip the cheka scan; zero disk reads for that path.
- `LogOffsets[path]` is the byte position after the last consumed `\n`. A
  partial trailing line (concurrent writer) is held back until it's
  newline-terminated. Offset > file size means truncation: rescan from 0.
- `/current` builds the tar fully in memory so a missing blob is a clean
  5xx, never a truncated stream after headers flushed.
- Pull merges log lines sorted by `(suggested_time, first_seen)`; local
  dedup is by line content hash. Writes are tmp-file + rename.
- `suggested_time` is client-side best-effort extraction: JSON keys
  `timestamp`/`ts`/`time`, RFC3339 or numeric epoch upscaled to ns by
  magnitude (`<1e11` s, `<1e14` ms, `<1e17` us).

**Review gate:**
- Why: a jailed agent writes its own home (hooks, settings.json, skills,
  memory). Before the gate the secretary pushed them and every non-pushOnly
  host applied them last-write-wins: one agent's hook or "cheat as a skill"
  ran fleet-wide with nobody looking (Prime Agent's RCON-skill failure,
  multiplied by the fleet).
- `syncclass.Classify(wire, mode)` is the only classifier; client and
  server both call it. Unknown agent, unknown path, executable bit or a
  non-prose extension inside a knowledge dir => privileged (fail closed).
  New allowlist entries ship as privileged until a rule names them.
- Privileged: archived (file_versions as always), never in a gated
  `/current`, refused client-side whatever a server sends, not approvable
  (403). Knowledge: served only as the effective approved version. The
  effective version is the newest approval whose version has no later
  rejection, so approve-old = rollback and reject-after-approve = withdraw
  (falls back to the previous approval).
- Client defence in depth: knowledge needs the server's `X-Cccp-Gate: 1`
  echo; pulled files lose exec bits; a symlink is never replaced
  (home-manager store links stay authoritative); a local knowledge file
  that differs from both the served and the last-approved sha is kept (the
  originating host's pending edit). Removal of knowledge needs an approved
  tombstone (`CCCP.deleted` PAX record), never absence; the tombstone loop
  only touches transcripts, so a gated snapshot without the privileged
  files an old client once pulled deletes nothing.
- `/log/since` and pull skip non-transcript log paths: a `.jsonl` under
  `hooks/` is not a transcript.
- Knowledge in an own space (`agent/<profile>-<space>/`) is archived, not
  reviewable (`syncclass.Reviewable`). It never enters the pending queue
  (`/review`, `review list`), `decide` refuses it (403), ingest runs no
  pre-check on it, and `review list --all`, the show page and pravda's file
  history label it `archived`. It stays in `file_versions` and in pravda.
  Why: an approval only makes the gated `/current` serve that version to
  clients whose prefix holds the path, and an own space's prefix belongs to
  the one home that writes it, which is the point of spaces. The Buzz agents,
  the only spaces so far, run `--push-only`, so an approval there would reach
  nobody, and their memories would bury the queue. Narrower rules were
  weighed and rejected (2026-10-28): matching `hermes-buzz-*` would hard-code
  one deployment into cccp and still queue a Buzz `SOUL.md`, which the Buzz
  definitions repository generates; a per-space policy in serve config would
  need a flag and module wiring on two servers (production and apiary's
  review cccp) for a single case. If several homes ever share a space on
  purpose and want their knowledge to fan out, add the policy then. The
  fleet Hermes (`agent/hermes/`), apiary-learning's proposals
  (`agent/apiary/`), claude and codex knowledge are queued as before.
  Approvals recorded before the rule still count for `/current`, and none
  exist in a space.
- Migration: `review_meta.gate_since` is stamped by the INSERT OR IGNORE in
  migrations.sql on the first open by a gate-aware binary. Versions at or
  before it are `legacy`: never served to gate-aware clients, not in the
  pending queue (`review list --all` shows them; approve one to serve it).
  Clients without the gate header get transcripts plus every other path
  frozen at `gate_since`, because an old client tombstones pulled files
  that vanish from the snapshot: dropping hooks from its tar would delete
  the home-manager symlinks it pulled over. So deploy order is free.
- Provenance is host-asserted: `host_id`, `sessions.json` and `state.json`
  live in the agent dir, which the agent can write. The reviewer judges the
  diff, not the label.
- The pre-check (`serve --review-precheck`) annotates only. Nothing in
  cccp approves automatically; a deployment that wants that wires a
  precheck that calls `cccp review approve` itself, explicitly.
- Config: `$XDG_CONFIG_HOME/cccp/<agent>.json` wins over
  `<dir>/cccp/config.json` because the latter is jail-writable (a planted
  `url` would send the host daemon's token elsewhere, a planted
  `allowlist` would walk outside the root). `token_file` keeps the token
  in the secret file; a hook-spawned daemon gets `--token-file`, never the
  token on argv.

**Agents:**
- `--agent NAME` (`CCCP_AGENT`, config key `agent`, default `claude`)
  selects a `Profile` from `internal/agent/profiles.go`: the default `--dir`
  and allowlist. Profiles register themselves from
  `internal/agent/profile_<name>.go` in `init()`; an unknown name is a hard
  error listing the registered ones. Two-pass resolution: the agent decides
  where `config.json` is looked for, and a file that sets `agent` re-derives
  the default dir afterwards.
- Every non-claude agent ships under the wire prefix `agent/<name>/`
  (`WirePrefix`). `Walk` applies it to `RelPath`, so push, `state.json`
  keys, tombstones and log offsets all live in wire space with no other code
  aware of it; pull maps wire paths back with `localRel`, which also makes
  claude ignore `agent/**` and every other agent ignore everything outside
  its own prefix. Singular `agent/` on purpose: `~/.claude/agents/` is a
  real Claude Code directory already in the allowlist.
- Own spaces: a non-claude name may carry a space after the first dash,
  `<profile>-<space>` (`hermes-buzz-fizz`, space chars `[a-z0-9._-]`), and
  ships under `agent/hermes-buzz-fizz/`. `syncclass.Kind` is the one rule
  that maps a name to its profile; the classifier, the client's profile and
  allowlist, pravda's converters, session filters, background and badge
  all go through it, so the server needs no list of spaces. Claude has no
  spaces (its wire root is the tree) and a malformed name has no kind, so
  it is privileged everywhere; profile names therefore never contain `-`
  (`RegisterProfile` panics). Why: several homes of one agent sharing
  `agent/hermes/` (13 agents on one host, 2026-10-27) replaced each
  other's `config.yaml` and `transcripts/.backfill.json` every minute
  (~15 k versions in a day, and `UNIQUE(path, version)` 500s that failed
  whole pushes), and their memories would have queued as versions of the
  fleet Hermes's `MEMORY.md`. The fleet's own `cccp-secretary-hermes-*`
  entries still pass the bare profile and keep sharing `agent/hermes/`.
- Push tombstones only paths inside its own prefix (`localRel`). A home
  that moves to another space (`hermes` -> `hermes-buzz-fizz`) forgets the
  old space's `KnownPushedBlobPaths` and offsets and re-ships its whole tree
  under the new prefix; it must never tombstone the paths it leaves, which
  other hosts still write, and which ingest's resurrection guard would then
  refuse from every host whose file is older than the tombstone.
- The same holds for a path that stops syncing: push tombstones a known path
  only while the allowlist still covers it and the profile's `Exclude` does
  not name it. A path that leaves the allowlist (Hermes `config.yaml`,
  2026-10-28) or enters `Exclude` (Hermes `transcripts/.backfill.json`) is
  forgotten: its versions stay on the server, the last one stays current,
  and hosts still on an older binary keep pushing theirs. Pull skips
  `Exclude` paths in both directions (never written, never removed as
  vanished), so an upgraded home neither takes an older home's copy nor
  loses its own.
- The secretary watcher is scoped to the allowlist (top-level dirs plus
  single files), not the whole root: a non-claude root is full of sqlite WAL
  churn that would fire a push every few seconds.
- A non-claude daemon minting its `host_id` copies `~/.claude/cccp/host_id`
  when present, so one machine is one host in pravda whatever the agent.
- Pravda: `internal/pravda/convert.go` wraps the `SessionStore`; a
  `Converter` registered per agent turns each raw line of an
  `agent/<name>/**/*.jsonl` file into zero or more Claude-Code-shaped lines
  before `ParseJSONL`/`Normalize` see them. Converters must be stateless per
  line: `parsedCache` relies on the output being append-only when the input
  is. Agents whose plugin already emits the Claude shape register an
  identity converter so the path is recognised.
  `RegisterSessionFilter(agent, keep)` narrows what the sidebar lists for an
  agent (codex: `sessions/`, `archived_sessions/`): every
  `agent/<name>/**/*.jsonl` is a log path, but not every log is a session.
- Plugins (hook wiring per agent) live in `plugin/<agent>/`; the nix package
  installs them at `$out/share/cccp/plugin/<agent>/` for sandboxed-agents to
  bind in.

**Watch:**
- The serializer reconciles every minute (push, then pull unless
  push-only), so missed watcher/SSE events and an idle host after an outage
  recover without another file write. Failed pushes retain the dirty flag
  for shutdown flushing.
- Partial log upload failures save acknowledged offsets and then return an
  error; callers must not treat an incomplete upload as success. Each
  upstream upload request has a five-minute deadline, independent of the SSE
  connection.
- Push and pull are serialized (shared `state.json`). One serializer
  goroutine drains a trigger channel and coalesces pending push/pull.
- Two echo layers: the SSE feed filters events whose host matches the
  subscriber's `X-Host-Id`; anything that slips through is neutralized by
  content-addressing (the re-push computes the same sha, `/ingest` reports
  skipped). One extra round-trip, no "just-wrote" caches.
- The broadcaster never blocks: slow SSE subscribers drop events. Safe
  because pulls are idempotent - the next tick covers it.
- Debounce is trailing-edge: no signal, no fire. So the daemon queues one
  push itself at startup: files written before it came up (a transcript from
  a session that ended, a host whose secretary was only just enabled)
  produce no fsnotify event and would sit unsynced until the next unrelated
  write (seen 2026-09-08).
- The serializer never pulls over unpushed local changes: the watcher sets a
  `localDirty` flag immediately (before the debounce fires), a pull trigger
  arriving while dirty is promoted to push-then-pull, and while the last
  push failed (cheka block, oversized blob, network) pulls are skipped until
  a push succeeds. Otherwise a remote SSE event lands mid-debounce and the
  pull reverts the just-saved edit with nothing left to recover it from.
- Session lifecycle owns daemon. Zero sessions = SIGTERM. On SIGTERM the
  serializer waits `finalPushGrace` for fsnotify to deliver, then runs one
  last push if `localDirty` is set or no push has succeeded yet: a one-shot
  session's last line is written milliseconds before the end hook fires, and
  the debouncer would take it to the grave. `hook-end` then waits up to 5 s
  for the pidfile to go, so a sandbox torn down right after the agent exits
  still gets the flush.
- `~/.claude/cccp` excluded from watcher (else state.json writes feed back).
- Watch defaults `--cheka-redact` to true unless a layer explicitly set it:
  a daemon that silently stops syncing is worse than redacted lines.
- Daemon files under `<root>/cccp`: `sessions.json` (+ `sessions.lock`
  flock), `secretary.pid`, `secretary.daemon.lock` (held for daemon
  lifetime), `secretary.log`.
- Liveness is tri-state via signal 0: live / gone / EPERM = "probably
  running, can't tell" (sandbox or other uid).

**Explore:**
- Two backends (`FSStore`, `SQLiteStore`), one `Store` interface.
- All HTML server-rendered. No build step. `//go:embed public/*`.
- WebSocket at `/ws/live/{path}` for live updates.
- `parsedCache` exploits append-only JSONL: only lines past the cached count
  get `json.Unmarshal`; a shrunken file clears the cache. Cache validity is
  a per-path unix-nano version stamped on every log event.
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
  much cheaper than diffing HTML. Changed entries go out as `hx-swap-oob`
  fragments keyed on `id="e-{Index}"`; no cached state or a shrink forces a
  full timeline replace.
- `Entry.Index` is 1-based; 0 is the template's "no inspector link"
  sentinel (sub-agent entries keep 0).
- `isHTMX` treats `HX-Boosted` as a full-page request: htmx swaps the whole
  `<body>` for boosted navs, so a fragment would wipe the sidebar.
- CORS middleware allows credentials only for an explicit allowlist:
  same-origin and localhost/127.0.0.1/::1 dev origins. Any other Origin gets
  no ACAO header. OPTIONS short-circuits before any auth. WS upgrades are
  same-origin only (empty Origin = non-browser, allowed).
- Sub-agent transcripts live under `<dir>/<session>/subagents/` -- flat
  `agent-<id>.jsonl`, or nested `workflows/<wf>/agent-<id>.jsonl` and
  `workflows/<wf>/journal.jsonl` since Workflow runs -- and `subagentRe`
  claims the whole subtree, so every such file lists under its parent
  session (label `wf › id`) instead of surfacing as a top-level session
  titled by a tool_result JSON blob. Sidechain events are attributed to
  their Task/Agent tool_use by walking `parentUuid` to the first
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
  with 3,339 user messages of 73 KB each rendered 247 MB before the cap. The
  timeline is also windowed: `?limit=N` (default `timelineWindow` = 400)
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
  default `project`) toggled by the chips under the search; groups are built
  per page from the paginated flat list (`groupItems`), ordered by first
  appearance so the newest activity stays on top, with sticky headers. The
  folder fallback for sessions without a cwd turns the Claude project slug
  back into a path so `-srv-repo` and `/srv/repo` land in one group.
- Codex child threads get a `child-session` marker entry in the parent
  timeline at the child's first timestamp (`childMarkers` + `mergeMarkers`;
  markers have Index 0 so `e-N` ids are untouched); the marker, like
  workflow `result` entries, lazy-loads the child transcript inline from
  `/subagent/<path>` via HTMX and links to the full page.
- `?agent=<name>` filters the list; chips with per-agent counts render above
  the list whenever more than one agent is present. Search also matches host
  and agent.
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
- Claude Code keeps adding event types. `fixtures/new-kinds.jsonl` carries
  one synthetic line per type seen in the wild and `newkinds_test.go`
  asserts each reaches a real renderer -- add a line there when a new type
  appears, and it will fail until a branch exists. To find them, group a day
  of live logs by `type` / `attachment.type` / `subtype`.
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
- `<bash-input>` / `<bash-stdout>` + `<bash-stderr>` (the `!` passthrough)
  are their own kinds; stderr renders in warning-orange beside stdout.
- MCP tools (`mcp__server__tool`) label as the bare tool name with a plug
  icon; the full name stays in the inspector.

- Background (`pravda/background.go`): the files a session started with
  are the synced memory / instruction / skill / command / agent / hook /
  settings paths of its agent (Claude project memory only for its own
  `projects/<slug>/`), each at `VersionTimeline.EffectiveAt(host, start)`:
  the newest version to have landed on that host by then -- its own writes
  at their mtime, pre-gate versions of other hosts at ingest, and the
  effective review approval at its decision time. Post-gate privileged
  files of other hosts never land. Known gap: a host keeps its own pending
  knowledge edit over a later approval, which the rule does not model.
  `instructions` / `nested_memory` attachments add what Claude reports it
  loaded, including project `CLAUDE.md` files outside the synced tree.
- File history: a version's writer is the session with a write tool call
  on that wire path from the same host within 15 min before / 2 min after
  its mtime, else the host-asserted live sessions recorded at push
  (`file_version_meta`), else root sessions of that host active then.
- `/sessions*`, `/files*` and `/index-panel/*` are pravda routes, so they
  sit behind `cookieAuth` (full or read token; `TestPushToken_*` lists them).
  The session page loads the panel with `hx-trigger="load"`, so the
  timeline render never waits for the background queries. Record ids are
  `<index id>#<Entry.Index>`; `sae_id` is `sae.RecordID(saeKind, saeText)`
  and `sae_scored` is a read of the current encoder's `sae_records` by
  primary key, only on `?records=1`.

**Responsive:**
- SAE: `sae_settings.go` supplies the shared settings/map payload. It
  includes every eligible message and all stored activations; browser
  display thresholds must not affect coverage. Missing imported records are
  distinct from low or absent activations. `sae.js` owns feature selection,
  colors, and display modes; dictionary defaults use localStorage and
  conversation overrides use sessionStorage, both scoped by encoder.
  `/sae/session/{path}` refreshes the map after live updates without
  broadcasting one viewer's encoder to other viewers. SAE and vote
  decoration operate on a request-local copy of cached entries. The index
  reports import availability, not external training progress.
- SAE dictionaries belong to one capture model and layer (`sae_encoders.model`,
  `layer`). Rows imported before the column existed are backfilled to
  `sae.LegacyModel` (Laguna layer 19) once, when the ALTER succeeds. Import
  takes both from `result.json`; `--model`/`--layer` only fill a legacy file
  and a disagreeing flag is an error. The on-demand scorer sends the
  dictionary's model and layer to `/v1/capture`, retries a bare 404 on
  `/v1/laguna/capture` (some proxies forward only that path), and
  fails a job permanently (`captureMismatchError`) on an unserved model, a
  501, or a reply naming another model or hidden size. Those messages are
  built from dictionary metadata and sanitized model ids only, never request
  text, because job errors are durable and logged.
- `/sae/import` installs the `sae.safetensors` a push carries into
  `--sae-models-dir` (`sae.InstallWeights`) before importing, and refuses the
  whole import (400) when the weights do not hash to `encoder_sha256`. The
  scale it installs is the pushed `scored.safetensors`'s, and only the first
  install of a dictionary counts: `nv-sae score` recomputes `scale` from the
  records it scored, so a nightly re-score of an older dictionary must never
  replace the training run's. Until 2026-11 nothing installed weights, and
  every pipeline dictionary was invisible to on-demand scoring
  (`Supports` false). The push token can install weights for a new encoder id,
  as it can already import one; the weights are only parsed as float tensors.
- SAE feedback: a message's vote is the sign of the sum of its `feedback`
  rows (the same text can be voted in several sessions), counted per
  dictionary by joining to that encoder's own `sae_records` and walking the
  stored `features` JSON with `json_each` -- driven by the small feedback
  table through the `(encoder, id)` key, never a scan of
  `sae_record_features`. The session hot path must not read `sae_records`
  (`TestSaeSessionCatalogDoesNotScanRecords`), so votes appear only on the
  features page, `/sae/feature/{id}` and the event inspector. Sort columns
  are server-side query params (`sort`, `sae_sort`, `*_dir`); unvoted
  features stay last on the ratio column in both directions.
- Three breakpoints. Above 1024px the sidebar is 300px. At <=1024px it
  narrows to 240px, the event label column drops 8rem -> 6rem, and the pager
  wraps (it clipped otherwise). At <=768px the sidebar becomes an off-canvas
  drawer.
- The drawer is a CSS-only `:has(#nav-toggle:checked)` pattern -- the
  checkbox and scrim live in `base.html` outside every htmx swap region, so a
  boosted nav does not reset it. `pravda.js` only closes it on session click.
- The header has a fixed 3.75rem height, so `.session-sub` is clamped to one
  ellipsized line on phones; wrapping pushed the title out of view.
- The tool status badge is `position:absolute; right:-24px`, which hangs off
  the full-width operation row on phones -- it goes back to inline flow there.

**Server/auth:**
- API endpoints accept bearer token, falling back to the `cccp_token`
  cookie. Pravda UI sits behind `cookieAuth`: OPTIONS passes through
  pre-auth (CORS must answer), programmatic callers (Authorization or
  HX-Request set) get 401, humans get 303 to `/login`.
- Three tokens: full, push (`PushToken`, push endpoints only, never reads)
  and read (`ReadToken`, `--read-token-file`). The read token exists so
  automation (the apiary self-learning loop) can read sessions and SAE
  views without being able to approve review-gate items: with the full
  token a loop could approve its own proposals. `readGate` wraps the whole
  outer mux; a request bearing the read token is served only for GET/HEAD
  on a `readRoutes` pattern (`internal/api/readtoken.go`, matched by a
  private `ServeMux`, so the semantics are the real router's) and then
  carries `readRoleKey` in its context, which `auth` and `cookieAuth`
  accept and `pushAuth` does not. Everything else is 403, so a new route is
  denied to the read token until listed. `decide` refuses the read role
  too, and the review page hides its buttons. The read token is
  bearer-only: never a cookie, never accepted by `/login`. Serve refuses a
  read token equal to the full or push token. `/sae/session/*` may enqueue
  scoring jobs for its encoder; that is derived data, not a write to the
  sync state. `readtoken_test.go` covers every route class for all three
  tokens.
- All-dup `/log/append` requests log at DEBUG - a well-behaved client tracks
  per-file offsets and never re-sends old lines.

**Cheka:**
- Scans on push only, client-side only. No server component. Pull is
  unscanned.
- Findings logged to stderr, grouped by (path, kind). Push fails if secrets
  found (`--cheka-allow` to adjust).
- Default: zero tolerance. Pass `-1` to disable.
- Pattern order matters: precise service patterns run before generic ones,
  and the first match claims its span - a JWT inside a Bearer header, or a
  key_value hit inside an already-claimed service span, yields one finding
  with the most specific kind.
- Generic patterns (connection_string, bearer, key_value, hex_token) denoise
  their first capture group: known examples, placeholder markers (also
  checked against the surrounding match, e.g. example.com hosts),
  template/format literals, env-var references, dictionary words, identifier
  shapes, Shannon entropy < 2.8.
- Candidate values are truncated at JSON escape sequences (`\n`, `\t`, ...) -
  regexes over quoted jsonl strings run past two-byte escapes.
- hex_token fires only above 64 hex chars (SHA-256 length is fine) and is
  entropy-filtered so binary dumps in session logs don't trigger.
- Redaction can't apply to blobs: it would change the content hash. Log
  lines are redacted in-flight; overlaps resolve left-to-right, longest match
  first at equal starts.
- Anthropic keys contain hyphens in the body; the generic `sk-` rule misses
  them, hence the dedicated pattern (regression-tested).

## Style

Pragmatic Go. Three similar lines > premature helper. No emojis, no verbose
docstrings. Default to no comments (run a comment stripper such as
github.com/eordano/strip-comments before committing; it strips `//` lines
including `//go:` directives - restore `//go:embed` in
`internal/store/store.go`, `internal/pravda/server.go`, and
`internal/pravda/render.go` if it eats them). The *why* lives in this file,
`docs/agents.md` and the man page, not in the code.

## Agents

Hermes, Codex and OpenCode internals (hooks, converters, allowlists,
backfill, local testing) are in [`docs/agents.md`](docs/agents.md). Read the
agent's section before touching `plugin/<agent>/`, `convert_<agent>.go`,
`profile_<agent>.go` or their fixtures.
