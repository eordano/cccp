# cccp (Hermes Agent plugin)

Pairs with `cccp watch --agent hermes`. Hermes keeps its sessions in
`state.db` (sqlite), which cannot be synced line-by-line, so this plugin
mirrors each session as a Claude-Code-shaped jsonl transcript that the
cccp log pipeline (append-only, deduped) and the pravda viewer already
understand:

- `pre_llm_call` -> one `user` line per turn (the first one becomes the
  session title).
- `post_tool_call` -> an `assistant` line with a `tool_use` block plus a
  `user` line with the matching `tool_result` (`tool_use_id` = Hermes'
  `tool_call_id`). Tool names are mapped to their Claude spellings so
  pravda's tool templates render them (`terminal` -> `Bash`, `read_file` ->
  `Read`, `write_file` -> `Write`, `patch` -> `Edit`, `search_files` ->
  `Grep` or `Glob` when `target=files`, `web_search` -> `WebSearch`,
  `web_extract` -> `WebFetch` (+`url`), `delegate_task` -> `Agent`
  (+`prompt`/`description`/`subagent_type` from the first task),
  `todo_list` -> `TodoWrite`, `skill_view` -> `Skill`, `clarify` ->
  `AskUserQuestion`); `path` gains a `file_path` alias and the original name
  is kept in the input as `hermes_tool`. Unmapped tools keep their name.
- `post_llm_call` -> an `assistant` line with the final text (and a
  `thinking` block when the history carries `reasoning`).
- `post_api_request` -> a `usage` line per API call:
  `{"type":"usage","model":...,"usage":{"input_tokens","output_tokens","cache_read_input_tokens","cache_creation_input_tokens","reasoning_tokens"}}`
  from the hook's `usage` summary, timestamped at `ended_at`, deduped on
  `api_request_id`. Hermes does not pass a cost to plugins, so live lines
  have no `costUSD`; pravda drops these from the timeline and sums them in
  the Environment panel.
- A `model-change` line (`{"type":"model-change","model":...,"effort":""}`)
  the first time a session reports its `model` and on every switch.
- User lines whose text is an injected prompt (a leading `<tag>`, or
  `[System`, `[Cron`, `[Scheduled`, `[Context`, `[Automated`,
  `[User attached`, `[The user attached`) carry `"isMeta": true`.
- `on_session_start` (or the first hook seen for a session) -> `cccp watch
  hook-start --agent hermes` with `{"session_id": ...}` on stdin: records
  the session and spawns the daemon if it isn't running.
- `on_session_finalize` -> `cccp watch hook-end --agent hermes`: drops the
  session and `SIGTERM`s the daemon once no Hermes sessions remain.

Transcripts land in `<HERMES_HOME>/transcripts/<session_id>.jsonl` (past
sessions too, see Backfill) and
ship server-side as `agent/hermes/transcripts/<session_id>.jsonl`. Lines
are deduped per session (a repeated `conversation_history` never re-emits),
tool payloads are capped at 512 KiB per block, and every hook is fail-open:
errors go to Hermes' logger, `cccp` missing from `PATH` is a no-op, and the
subprocess calls time out after 10 s.

## Install

```sh
# cccp on PATH
go install github.com/eordano/cccp/cmd/cccp@latest

# plugin into the Hermes home
ln -s "$(pwd)/plugin/hermes" ~/.hermes/plugins/cccp
hermes plugins enable cccp      # adds `cccp` to plugins.enabled in config.yaml
```

The sandboxed-agents `hermes` wrapper does both for you (see its README).

## Config

`~/.hermes/cccp/config.json` (same four-layer resolver as the Claude side,
`--agent hermes` selects `~/.hermes`, or `$HERMES_HOME` when set):

```json
{
  "url": "https://sync.example",
  "token": "...",
  "agent": "hermes",
  "push_only": true
}
```

What syncs: `transcripts/`, `memories/`, `SOUL.md`, `config.yaml`. Never: `state.db`, `.env`, `auth.json`, caches.

## Backfill

Sessions that predate the plugin (or ran without it) live only in
`<HERMES_HOME>/state.db`. On `register()` the plugin starts a daemon thread
that, after a 5 s delay, opens `state.db` read-only (Hermes' own
`SessionDB(read_only=True)`; plain `mode=ro` sqlite if `hermes_state` is not
importable) and writes the same Claude-shaped transcript for every past
session, so the daemon ships them like any other file under `transcripts/`.

- Line shape is identical to the live hooks (same emit helper); `uuid`s are
  `uuid5(session_id, message id, tag)` so a re-run never duplicates a line,
  `timestamp` is the message's own time, `cwd` the session's recorded cwd.
  Tool names get the same Claude mapping and user rows the same `isMeta`
  rule as the live hooks. `state.db` keeps no per-call usage, so the
  backfill writes one `model-change` (the session's `model`, at the earlier
  of `started_at` and the first message) and one `usage` line per
  `session_model_usage` row (per model and task, e.g. `background_review`,
  `title_generation`; `costUSD` = actual cost, else the estimate), falling
  back to the `sessions` row's aggregate counters on older schemas. These
  are session totals, not per-turn steps.
- Transcripts backfilled before these lines existed are not touched on a
  normal run (the stamp's `last_id`/`size` match); deleting
  `transcripts/.backfill.json` appends the new `model-change`/`usage` lines
  at the end of each file (uuid dedup keeps everything else), while lines
  already written keep their old tool names and lack `isMeta`.
- `transcripts/.backfill.json` records `last_id`/`size` per session; a later
  launch only touches sessions with new messages. Deleting the stamp is
  safe: existing lines are deduped on `uuid`.
- Skipped: sessions with no messages, the sessions this process' live hooks
  own, sessions with no `ended_at` and a message in the last 15 min (likely
  live in another Hermes), and transcripts the live hooks wrote (a file with
  none of our deterministic uuids is left alone forever).
- Each run is capped at 20 s and resumes on the next launch; a
  `transcripts/.backfill.lock` flock keeps two Hermes processes from
  backfilling at once. Everything is fail-open and logged via the Hermes
  logger. `CCCP_BACKFILL=0` disables the automatic run.

Manual entry points:

```
/cccp backfill [seconds]     in-session; 0 = no time budget
/cccp status                 stamp summary and last run
python3 ~/.hermes/plugins/cccp/__init__.py --backfill [HERMES_HOME] [--budget S] [--no-api] [-v]
```

The CLI prints one JSON stats line (`sessions`, `written`, `lines`,
`skipped`, `active`, `empty`, `foreign`, `resume`, `api`). Tests:
`python3 plugin/hermes/test_backfill.py` (the Hermes-API cases run when
`hermes_state` is importable, e.g. `PYTHONPATH=<hermes source>` with Hermes'
interpreter).

## Troubleshooting

- Daemon log: `~/.hermes/cccp/secretary.log`
- Status: `cccp watch status --agent hermes`
- Stop: `cccp watch stop --agent hermes`
- Session list: `~/.hermes/cccp/sessions.json`
- Plugin not loading: `hermes plugins list` must show `cccp` enabled; the
  manifest lives at `~/.hermes/plugins/cccp/plugin.yaml`.
