# CLAUDE.md - cccp

Product docs: [`README.md`](README.md), [`man/cccp.1`](man/cccp.1).
This file is what's not obvious from reading the code.

## What this is

Cloud code config provider. A tool to sync `~/.claude` across machines.

Claude Code doesn't have built-in cloud sync. If you use it on more
than one computer, or across hosts and VMs, you know the pain. cccp
syncs memories, project notes, hooks, skills, settings, and
conversational history between devices. It is a small Go server and CLI
that keeps them in sync.

Sync has three modes depending on file type. Files like skills and
memories use a last-write-wins policy. Files ending in `.jsonl` (like
conversation logs) use append-only dedup. Deleted files are tracked
through tombstones.

The binary uses as few dependencies as possible. Everything ships as
one binary: `cccp serve` for the server, `cccp push`/`pull` for sync,
`cccp watch` for the background daemon that watches your Claude
directory and pushes changes automatically, and `cccp explore` for a
standalone session viewer with live updates. There is also a secret
scanner (cheka) built into push that detects API keys, tokens, or
passwords locally before anything leaves the machine.

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
Raw `http.ServeMux`, single-writer SQLite (`SetMaxOpenConns(1)`),
`log/slog` to stderr.

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
  `BlobCache` entry; unreadable log files keep their prior offset. The
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
  that line forever.
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

**Watch:**
- Push and pull are serialized (shared `state.json`). One serializer
  goroutine drains a trigger channel and coalesces pending push/pull.
- Two echo layers: the SSE feed filters events whose host matches the
  subscriber's `X-Host-Id`; anything that slips through is neutralized by
  content-addressing (the re-push computes the same sha, `/ingest`
  reports skipped). One extra round-trip, no "just-wrote" caches.
- The broadcaster never blocks: slow SSE subscribers drop events. Safe
  because pulls are idempotent - the next tick covers it.
- Debounce is trailing-edge: no signal, no fire.
- The serializer never pulls over unpushed local changes: the watcher
  sets a `localDirty` flag immediately (before the debounce fires), a
  pull trigger arriving while dirty is promoted to push-then-pull, and
  while the last push failed (cheka block, oversized blob, network)
  pulls are skipped entirely until a push succeeds. Otherwise a remote
  SSE event lands mid-debounce and the pull reverts the just-saved edit
  with nothing left to recover it from.
- Session lifecycle owns daemon. Zero sessions = SIGTERM.
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
- Sub-agent transcripts live at `<dir>/<session>/subagents/agent-<id>.jsonl`
  and are lazy-loaded via HTMX; sidechain events are attributed to their
  Task/Agent tool_use by walking `parentUuid` to the first non-sidechain
  ancestor.

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
