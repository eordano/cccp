# proofs/ -- a machine-checked model of cccp's sync design

`cccp.v` is a Coq development that models what the Go code **does** (not what
the product README promises). Parts 1-5 prove, with no `Admitted` and no
`Axiom`, that the **server-side stores** do not lose data. Parts 6-7 are
*honesty lemmas*: machine-checked counterexamples showing which preconditions
of Parts 4-5 the **client** code fails to establish, and which client
behaviors the positive theorems do not endorse. Read the two halves together:
every "safe" theorem in the first half is a conditional, and the second half
proves which conditions the code actually discharges (few) and which it does
not (most of the interesting ones).

## Machine-checked: yes

Checked with The Rocq Prover 9.1.1 (nixpkgs `coq` 9.1.1 + `rocq9.1-stdlib`
9.0.0), exit 0, zero warnings from the file itself:

```sh
STDLIB=$(nix build --no-link --print-out-paths nixpkgs#coqPackages.stdlib)
ROCQPATH="$STDLIB/lib/coq/9.1/user-contrib" nix shell nixpkgs#coq -c coqc cccp.v
```

`Print Assumptions` on all **43 theorems** reports **"Closed under the global
context"** -- the development is axiom-free end to end (supporting `Lemma`s
such as `race_final` are not counted).

Portability note: the file uses `From Stdlib Require Import ...` (Rocq 9 /
Coq >= 8.20 stdlib prefix). For Coq 8.16-8.19 replace `From Stdlib` with
`From Coq` on the four Require lines; nothing else is version-sensitive.

## Model -> code map

All paths relative to the repository root. Line numbers as of the tree this was
written against.

### Part 1 -- blob store (per-path append-only version list)

| Coq | Go it abstracts |
|---|---|
| `entry` = `Content sha \| Tomb` | `internal/store/store.go:21-29` (`FileVersion.ContentSHA *string`; nil => NULL `content_sha` = tombstone); `internal/api/ingest.go:63` (`isDelete := e.SHA == ""`), `ingest.go:102-105` |
| version list, position = server version, append at end | `internal/api/ingest.go:90-93` (`nextVersion = latest.Version + 1` -- **arrival** order); `internal/store/migrations.sql` `file_versions` PK `(path, version)` |
| `last_opt` | `internal/store/store.go:69-85` `LatestVersion` (`ORDER BY version DESC LIMIT 1`) |
| `snapshot` | `internal/store/store.go:158-182` `CurrentSnapshot` (`MAX(version)` per path, `WHERE content_sha IS NOT NULL`) |
| `ingest1` (skip-if-equal-to-latest, else append) | `internal/api/ingest.go:80-110`; equality test `sameContent` `ingest.go:130-135` |
| `ingest_seq` (manifest = fold of entries) | `internal/api/ingest.go:46-111` scanner loop |
| `entry_available`, `ingest_gated` (two-phase) | `internal/api/ingest.go:64-77` (`HasBlob` miss => `need`/`pending`, entry **not** committed); client re-send: `internal/agent/push.go:181-203` |
| client-generated tombstone entries | `internal/agent/push.go:87-92` (`KnownPushedBlobPaths` minus current walk => `SHA:""`), state in `internal/agent/state.go:11-17` |

Theorems:

- **`ingest_append_only`, `ingest_preserves_reads`, `ingest_seq_append_only`,
  `gated_append_only`** (Theorem 1): ingest only ever appends a suffix;
  every version readable before is readable, unchanged, after. Formal shadow
  of `AppendVersion` being a bare `INSERT` (`store.go:87-97`) -- `store.go`
  contains no UPDATE/DELETE on `file_versions` at all.
- **`dedup_skip_sound`**: skipping an entry equal to the latest version yields
  the same snapshot as appending it would have (`ingest.go:85-88`).
- **`push_content_visible` / `push_delete_hides`**: after ingesting a
  content/delete entry the snapshot is that content / absent -- whether it
  committed or was deduped.
- **`missing_blob_not_committed`, `resend_after_upload_commits`**: the
  two-phase protocol parks entries with missing blobs without touching the
  version list, and the re-sent manifest commits normally.
- **`no_resurrection`, `recreate_wins`, `push_after_tombstone_wins`**
  (Theorem 4): once a tombstone exists, the snapshot excludes the path unless
  a *later* Content version exists; a later push re-creating the file wins.
  **Caution:** these are server-side facts about the version list. The
  *client* pushes with no tombstone check, so "a later Content version"
  arrives from any stale host that pushes before pulling -- see
  `push_before_pull_resurrects` (Part 7, D1). Nothing here means "a deleted
  file stays deleted".

### Part 2 -- honesty lemma A: no mtime-LWW

| Coq | Go |
|---|---|
| `fversion` (entry + mtime), `ingest_m`, `msnapshot` | `internal/api/ingest.go:94-101` stores `e.MtimeNS`; **nothing** in `store.go`/`ingest.go` ever orders or compares by mtime |

- **`server_ignores_mtime`**: whatever mtimes the history carries, the last
  arrival that commits determines the snapshot.
- **`no_mtime_lww_counterexample`**: a concrete interleaving where a push with
  mtime 5 arrives after a push with mtime 10 and wins -- the *older* file
  overwrites the *newer* one. This behavior is reachable; nothing in the
  server prevents it.
- Lemma A is only the **server half** of "concurrent edits race". The client
  half -- pull overwrites/deletes local files with no local-modification
  check, even when the preceding push failed -- is Part 7, D3
  (`watch_reverts_unpushed_edit`).

### Parts 3-4 -- log store (G-set) and pull cursor

| Coq | Go |
|---|---|
| `key` = (path, line_hash); `lline` with immutable `lseen` | `migrations.sql` `log_lines` PK `(path, line_hash)`; `internal/api/log.go:57-64` (hash = sha256 of the line, `FirstSeen = time.Now().UnixNano()`) |
| `insertL` (insert-if-absent) | `internal/store/store.go:123-136` `AppendLogLine` = `INSERT OR IGNORE` |
| `pull c st` = `{ l \| first_seen l > c }` | `internal/store/store.go:138-156` `LogLinesSince` (`WHERE first_seen > ?`, strictly greater); served by `internal/api/log.go:115-150` |
| cursor | `internal/agent/state.go:14` `LogCursor`; `internal/agent/pull.go:41-44` (`--full` => since 0), `pull.go:65-75` (advance guarded by `newCursor > state.LogCursor`, `SaveState` only after every merge succeeded), `pull.go:199-215` (`maxFirstSeen` of the completed pull) |
| `merge_nonfull` | `internal/agent/pull.go:250-277` `mergeLogFile` non-full path -- **idealized**: the model dedups against the complete local line set; the code dedups only against what its scanner managed to read (see Part 6, C5) |
| `full_rewrite` | `internal/agent/pull.go:246-248` + `rewriteLogFile` `pull.go:280-306` (local file replaced by server-derived lines) |

Theorems:

- **`log_dedup_sound`** (Theorem 3): inserting a duplicate (path, line_hash)
  leaves the store *literally* unchanged -- in particular the stored
  `first_seen` is immutable.
- **`merge_idem`, `merge_comm`, `merge_assoc`** (Theorem 3, CRDT G-set):
  merge is idempotent, commutative and associative under key-set equality.
  (Key-level = content-level because `line_hash` is sha256 of the contents;
  the model, like any hash-based dedup, implicitly assumes sha256 injectivity
  on the lines that occur. `first_seen` metadata is first-writer-wins and is
  *not* claimed commutative.) These hold for the **server** store. The
  client-side *file* merge is not `insertL` and is **not** idempotent when
  the dedup scan truncates -- Part 6, C5.
- **`pull_no_loss`, `union_covers`** (Theorem 2): every line with
  `first_seen > c` is in `pull(c)`; local ∪ pull(c) covers all of them.
- **`covered`** is the client invariant that `LogCursor` *means*: every server
  line at or below the cursor is already local.
- **`completed_pull_converges`**: under the invariant, one completed pull
  makes local ⊇ the whole server set.
- **`cursor_monotone`, `cursor_advance_safe`**: advancing the cursor to the
  max `first_seen` of a completed pull never moves backwards and
  re-establishes the invariant -- repeated pulls converge.
- **`interrupted_pull_safe`**: an aborted pull (cursor untouched, arbitrary
  partial data written) preserves the invariant -- cursor-on-completion is
  what makes interruption safe. The client honors this for every error *it
  can see* (any error in `pullLogs`/`mergeLogFile` returns before
  `SaveState`, `pull.go:65-75`). **It cannot see a truncated `/log/since`
  stream** -- the server error path after headers are flushed produces a
  well-formed prefix (`log.go:142-145`) that the client treats as a
  completed pull. That case is Part 6, C4.
- **`cursor_skip_unsafe`** (honesty): advancing the cursor *without*
  completing the pull provably breaks the invariant. Reachable in the
  shipped code via the truncated-stream case above.
- **`append_preserves_covered`** + **`clock_regression_unsafe`** (honesty):
  new server lines keep the invariant *only if* their `first_seen` exceeds
  the client cursor. That is an **assumption the code does not enforce**:
  a line stamped at or below an already-issued cursor is provably invisible
  to every future incremental pull -- only `pull --full` recovers it. Two
  ways to get there: a server wall-clock regression (NTP step), **or -- with
  a perfectly monotone clock -- the stamp/insert race** of Part 6, C3:
  `FirstSeen` is evaluated in the handler loop (`log.go:57-63`)
  non-atomically with the `INSERT` (`log.go:66`); `SetMaxOpenConns(1)`
  (`store.go:37`) serializes statements, not stamp+insert pairs.

### Part 5 -- honesty lemma B: `pull --full`

- **`nonfull_merge_preserves_local`, `nonfull_merge_covers_incoming`**: the
  default merge never touches existing local lines and lands every pulled
  line (under the complete-dedup idealization; C5 for the deviation).
- **`full_pull_safe_if_pushed`**: *set-preservation of the rewrite itself*,
  under the precondition "every local line is on the server". **The
  precondition is not established by a completed `cccp push`** -- see C1
  (redaction), and push additionally warn-and-skips failing log files
  (`push.go:218-222`), skips unreadable blobs (`push.go:112-117`), and holds
  back a non-newline-terminated trailing line (`push.go:339-343`). And even
  when the precondition holds, `--full` corrupts the *next* push via stale
  offsets -- C2.
- **`full_pull_lossy_if_unpushed`**: if any local line is *not* on the
  server, the rewrite provably destroys it.
  (Code nuance: `--full` only rewrites paths that appear in the server
  response (`pull.go:220-233` iterates `byPath`), so a log file the server
  has never heard of survives; the loss precondition is per-path -- unpushed
  lines in a file for which the server *does* have lines.)

### Part 6 -- honesty lemmas C: the log pipeline vs its own preconditions

Each lemma is a concrete, machine-checked failing interleaving of the actual
code paths.

| Coq | Go it exhibits |
|---|---|
| `redact_line`, **`redacted_push_not_subset`** (C1) | `push.go:355-358` sends `cheka.RedactLine(line)`; `log.go:57-60` keys the server line by the redacted content's hash; `cccp watch` defaults `--cheka-redact` to true. After a fully *successful* push, the local original is provably ∉ server -- `--full` destroys it. |
| `flat_file`, `split_nl`, `push_payload`, **`stale_offset_fragment`** (C2) | `Pull` never touches `state.LogOffsets` (`pull.go:34-77`); `rewriteLogFile` (`pull.go:280-306`) re-materializes the file in server sort order `(suggested_time, first_seen)` (`pull.go:221-226`). The preserved byte offset -- a line boundary of the old file -- lands mid-line in the new one; the next push seeks there (`push.go:330-334`) and commits the suffix fragment as a permanent, brand-new server line (it hashes differently). The counterexample is byte-level: old file `[[1;2;3]]`, offset 4, rewritten file `[[7;7];[1;2;3]]` => pushed "line" `[2;3]`, a member of neither file. |
| `op`/`wstate`/`wstep`/`wrun`, `race_trace`, **`monotone_stamp_race_unsafe`** (C3) | `FirstSeen = time.Now().UnixNano()` assigned in the handler loop (`log.go:57-63`) non-atomically with the `INSERT` (`log.go:66`); `SetMaxOpenConns(1)` (`store.go:37`) serializes statements only. Trace: A stamps t=1 · B stamps t=2 · B inserts · a pull completes (cursor 2) · A inserts. A's line is <= cursor forever -- `clock_regression_unsafe`'s state with **strictly increasing** timestamps. |
| **`truncated_stream_tie_skipped`** (C4) | `handleLogSince` streams after headers are flushed; a mid-stream store error just returns (`log.go:142-145`). The client sees a well-formed ndjson prefix, treats the pull as completed, and advances the cursor (`pull.go:199-234`, `69-71`). `first_seen ASC` ordering rescues strictly-greater lines; a line *tying* the truncation point is <= cursor forever. |
| `merge_nonfull_partial`, **`partial_dedup_duplicates`** (C5) | `mergeLogFile` ignores `sc.Err()` (`pull.go:251-260`): a local line > `MaxScanLine` (4 MB, `walk.go:16`) or any read error silently truncates the `existing` dedup set; pulled duplicates are then appended. File-level `merge_idem` fails. |
| `iterate`, `commit_prefix`, `push_attempt_off`, **`oversize_line_wedges_push`** (C6) | Client push has *no* line-length limit (`io.ReadAll` + `bytes.Split`, `push.go:335-350`); the server scanner caps at `maxScanLine` = 4 MB (`api/server.go:20`, `log.go:41-42`). Oversize line => scanner stops, request 400s (`log.go:78-81`) -- *after* committing earlier lines of the same request (per-line INSERTs, no transaction). Client keeps the old offset (`push.go:218-222`) and replays the identical payload forever: log sync for that file is permanently wedged. |
| `pull_bounded`, **`oversize_line_wedges_pull`** (C7) | Mirror image: the client scanner reading `/log/since` caps at `MaxScanLine` too (`pull.go:200-201`), and JSON escaping can ~double a line on the wire -- a ~3 MB line that pushed fine can be impossible to pull. `pullLogs` errors before merging anything or advancing the cursor (`pull.go:216-218`, `65-68`): every pull, on every client, dies at the same line. |

C6/C7 are **liveness** failures -- the total folds of Parts 3-4 cannot even
express them, which is why they carry their own lemmas. Any discipline that
depends on "pushes and pulls complete" is void while one of these wedges is
in place.

### Part 7 -- honesty lemmas D: client-side blob behaviors

| Coq | Go it exhibits |
|---|---|
| **`push_before_pull_resurrects`** (D1) | Push builds its manifest from the walk alone (`push.go:94-133`) and generates tombstones only from its *own* `KnownPushedBlobPaths` (`push.go:87-92`) -- no check against server tombstones. Any host that still holds the old copy and pushes before pulling re-uploads it; arrival order makes it the snapshot. Nobody re-created anything. |
| **`server_reset_indistinguishable`**, `swept_away`, **`server_reset_mass_delete`** (D2) | The client has no server-epoch check (`state.go`, `pull.go`). Against an empty/reset/restored-from-backup DB, `/current` is empty and `pull.go:50-59` `os.Remove`s **every** path in `KnownPulledBlobPaths`. Model-level, per path, a fresh server and a fully tombstoned one are the *same observation* -- the sweep is sound inside the model and a mass delete against the wrong reality. Files never pulled or pushed are not in `KnownPulledBlobPaths` and survive. |
| `apply_pull`, `serializer_cycle`, **`watch_reverts_unpushed_edit`** (D3) | Pull is remote-wins, unconditionally: `writeIfDifferent` (`pull.go:126-160`) overwrites and the tombstone sweep (`pull.go:50-59`) deletes, with no local-modification check. The watch serializer runs `Pull` even when the preceding `Push` **failed** -- the push error is only logged (`serializer.go:36-49`). A cheka finding or a dead network blocks the push; the next pull reverts or deletes the unpushed local edit. |
| `walk_result`, `push_or_abort`, **`dangling_symlink_wedges_push`** (D4) | A symlink whose target cannot be resolved errors in `resolveAndStat` (`walk.go:110-118`), propagates out of `WalkDir` (`walk.go:90-100`), and `Push` returns before anything is sent (`push.go:63-66`). Unlike unreadable regular files (warn-and-skip), one broken link aborts the *entire* push, every retry, machine-wide, until removed. |

## Operational discipline implied by the honesty lemmas

Parts 6-7 changed these rules; the old list overclaimed. What the theorems
actually support:

1. **Treat `pull --full` as destructive, always.** There is *no* push-based
   procedure that makes it safe:
   - a completed `cccp push` does **not** put every local line on the server
     (`redacted_push_not_subset`; plus skipped log files/blobs and held-back
     trailing lines -- all reported as warnings, not failures);
   - with redaction on (the `cccp watch` default), the local originals of
     redacted lines are *guaranteed* casualties;
   - even when nothing is lost, `--full` leaves `state.json`'s `LogOffsets`
     pointing into the pre-rewrite file, and the next push commits a garbage
     fragment as a permanent server line (`stale_offset_fragment`).
   If you must run it: stop `watch`, accept the loss of local-only and
   redacted lines, and afterwards clear `LogOffsets` in
   `~/.claude/cccp/state.json` (or delete the state file and accept a full
   re-push) before the next push.
2. **There is no universally safe "push before you pull".** Pushing first
   protects *your* unpushed blob edits from D3-style overwrites, but it also
   **resurrects other hosts' deletions** (`push_before_pull_resurrects`):
   the manifest has no tombstone check. After deleting a synced file on one
   host, pull on every other host *before* its next push, or the deletion
   comes back. `cccp watch` everywhere shrinks this window (deletions
   propagate promptly) but cannot close it, and watch itself pulls after
   failed pushes (`watch_reverts_unpushed_edit`) -- do not treat it as a
   safety mechanism for local edits.
3. **Never hand-advance the cursor; only completed pulls may move it**
   (`cursor_skip_unsafe`, `interrupted_pull_safe`): the client code does
   this for every failure it can detect. Two server-side cases defeat it --
   a truncated `/log/since` stream (`truncated_stream_tie_skipped`) and the
   stamp/insert race (`monotone_stamp_race_unsafe`) -- so an incremental
   cursor can permanently skip lines *without any client bug or clock step*.
4. **Server clock hygiene (chrony/timesyncd) is necessary but NOT
   sufficient** for cursor completeness: `clock_regression_unsafe`'s state
   is reachable on a monotone clock (`monotone_stamp_race_unsafe`). The
   dedup key means nothing is lost *server-side*; the gap is client
   visibility. The only recovery for a skipped line is `pull --full` -- with
   all of rule 1's costs. Fixing this properly needs a code change (stamp
   inside the insert, or issue cursors from committed max, not wall clock).
5. **Deletions are NOT safe by construction.** Server-side, tombstones are
   ordinary appended versions and history stays readable
   (`no_resurrection`, `ingest_preserves_reads`). But "a deleted file stays
   deleted until someone re-creates it" is **false**: any stale host that
   pushes before pulling resurrects it (D1), and conversely a pulled
   tombstone deletes a locally *modified* file with no warning (D3).
6. **Watch the liveness wedges** -- while any of these holds, every rule
   above that depends on a push or pull completing is unmeetable:
   - a >4 MB line in a local `.jsonl` wedges that file's log sync forever
     (`oversize_line_wedges_push`) and silently strands all lines behind it;
   - a stored line whose JSON encoding exceeds 4 MB wedges **every pull
     fleet-wide** (`oversize_line_wedges_pull`);
   - one dangling symlink under the allowlist aborts every push from that
     machine (`dangling_symlink_wedges_push`).
7. **Never point clients at a reset or restored server** without first
   clearing `KnownPulledBlobPaths` (or the whole state file) on every
   client: an empty `/current` is indistinguishable from "everything was
   deleted" and triggers a local mass delete (`server_reset_mass_delete`).
   The server DB is the only immortal copy the model believes in; treat it
   that way (back it up, never start clients against a fresh one that
   claims to be the old one).

## Deliberate model simplifications

- SHAs, paths and hashes are `nat`; content is identified with its hash
  (sha256 collision-freedom is assumed, as it is by the code's dedup).
  Redaction (Part 6, C1) is any function on contents; bytes in C2 are nats
  with newline = 10.
- One path's version list is modeled (the store is a per-path map; paths are
  independent because every query in `store.go` is keyed or grouped by path).
- HTTP *transport* and SQLite statement atomicity are trusted: "the server
  executes `INSERT`" is the atomic step. Single-writer SQLite
  (`store.go:37`, `SetMaxOpenConns(1)`) justifies modeling ingest as a
  sequential fold. Note this trust does **not** extend to server *code*
  behavior over HTTP: the truncated-stream case (C4) and the stamp/insert
  race (C3) are in-scope and modeled.
- Blob-side pull is modeled only at snapshot level plus the
  `KnownPulledBlobPaths` sweep (D2/D3). Local files the client never pulled
  or pushed are not in `KnownPulledBlobPaths` and are never deleted by the
  sweep -- but see D2 before drawing comfort from that.
- `cccp watch` is modeled only as the serializer's push-then-pull-regardless
  cycle (D3); debounce, SSE echo suppression and session lifecycle are not
  modeled.

## Mismatches / observations noticed while formalizing

1. **Metadata-only changes never sync.** `sameContent` (`ingest.go:130-135`)
   compares only the content sha -- a chmod- or mtime-only change is counted
   as `skipped` and creates no version. The model mirrors this (entry
   equality is sha equality). Not data *loss*, but surprising: mode changes
   propagate only when the file's bytes next change.
2. **The incremental cursor can skip lines without any clock anomaly**
   (`monotone_stamp_race_unsafe`, C3): `first_seen` is stamped before, and
   non-atomically with, the insert. An earlier draft of this README
   attributed `clock_regression_unsafe` exclusively to NTP steps backwards;
   that was wrong -- the state is reachable on a monotone clock.
3. **A `/log/since` response can be silently truncated** (`log.go:142-145`,
   C4): headers are already flushed, so the client sees a well-formed prefix
   as a completed pull and advances its cursor over `first_seen` ties at the
   truncation point.
4. **`--full` semantics are per-path** (Part 5 note) -- the flag is more
   dangerous the more the server already knows about a file, which is the
   opposite of the intuition "full = safest resync". It also strands
   `LogOffsets` (C2): the *next* push after a `--full` can manufacture
   garbage server lines.
5. **`cccp push` succeeding does not mean everything was pushed**: failing
   log files (`push.go:218-222`), unreadable blobs (`push.go:112-117`) and
   non-newline-terminated trailing lines (`push.go:339-343`) are all
   skipped with at most a warning; redaction (`push.go:355-358`) replaces
   line contents in-flight. The success exit code is about the run, not
   about coverage.
6. **The client merge is weaker than the G-set** (C5): `mergeLogFile`
   ignores `sc.Err()`, so its dedup set can silently truncate and the merge
   duplicates lines on disk -- file-level idempotence fails even though the
   server-side `merge_idem` holds.
7. The cursor guard `newCursor > state.LogCursor` (`pull.go:69-71`) makes a
   `--full` pull (since 0) unable to move the cursor backwards -- matches
   `cursor_monotone`.
8. **Scanner bounds are asymmetric by direction** (C6/C7): push has no line
   limit but the server reads with one; pull reads with one against
   JSON-inflated lines the server happily stored. Both directions wedge:
   one file's sync (push side) or the whole fleet's pulls (pull side).
