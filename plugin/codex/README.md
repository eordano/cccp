# cccp (Codex CLI hooks)

Pairs with `cccp watch --agent codex`. Codex 0.153.4 runs Claude-style
lifecycle hooks from `$CODEX_HOME/hooks.json` (default `~/.codex/hooks.json`);
this directory is that file:

- `SessionStart` -> `cccp watch hook-start --agent codex` - records the
  session id and spawns the daemon if it isn't already running.
- `SessionEnd` -> `cccp watch hook-end --agent codex` - drops the session id
  and `SIGTERM`s the daemon once no Codex sessions remain.

Both commands are wrapped in `command -v cccp ... ; cat >/dev/null`, so a
machine without `cccp` on PATH gets a silent no-op instead of a hook error.

The daemon syncs the codex home (`sessions/` rollouts, `history.jsonl`,
`session_index.jsonl`, `config.toml`, `hooks.json`, `AGENTS.md`, `rules/`,
`skills/`; never `auth.json`, the sqlite stores, caches or tmp) to the
server under the wire prefix `agent/codex/`, and pravda renders the rollouts
through the codex converter.

## Install

```sh
cp plugin/codex/hooks.json ~/.codex/hooks.json   # or $CODEX_HOME/hooks.json
cat > ~/.codex/cccp/config.json <<EOF
{ "url": "https://sync.example", "token": "...", "agent": "codex" }
EOF
```

`agent` in `config.json` (or `CCCP_AGENT=codex`) is what makes a bare
`cccp push` / `cccp watch status` from a codex home resolve to that home.

## Hook trust

Codex refuses to run hooks it has not seen before. The first interactive
`codex` after installing the file shows "Hooks need review"; pick *Trust all*
(or review each: the commands are the two lines above). Trust is persisted in
`config.toml` as `[hooks.state."<abs path>/hooks.json:session_start:0:0"]
trusted_hash = "sha256:..."`, keyed on the file's absolute path and content,
so editing `hooks.json` or moving the codex home re-prompts. Non-interactive
`codex exec` never prompts: it skips untrusted hooks silently unless run with
`--dangerously-bypass-hook-trust`.

## Why the root `hooks.json` and not a Codex plugin

At 0.153.4 the `hooks` feature is stable and on by default (`codex features
list`); there is no `codex_hooks` config flag to set. The `plugin_hooks`
feature is *removed* in this release, so a `$CODEX_HOME/plugins/<name>/hooks/hooks.json`
+ `.codex-plugin/plugin.json` bundle would install but never fire. The root
file is the only vehicle that runs.

## Payloads

Hooks receive one JSON object on stdin (no trailing newline):
`session_id`, `hook_event_name`, `cwd`, `transcript_path` (the rollout file);
`SessionStart` adds `model`, `permission_mode`, `source` (`startup`, `resume`,
`clear`, `compact`), `SessionEnd` adds `reason`. `SessionStart` fires when the
first turn is submitted, not at process start, and `SessionEnd` timeouts are
clamped to 3 s by codex - hence the `"timeout": 3`.

## Troubleshooting

- Daemon log: `~/.codex/cccp/secretary.log`
- Status: `cccp watch status --agent codex`
- Stop: `cccp watch stop --agent codex`
- Session list: `~/.codex/cccp/sessions.json`
