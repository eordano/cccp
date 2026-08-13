# cccp (Claude Code plugin)

Pairs with `cccp watch`. The plugin only wires hooks:

- `SessionStart` -> `cccp watch hook-start` - records the session id and
  spawns the daemon if it isn't already running.
- `SessionEnd` -> `cccp watch hook-end` - drops the session id and
  `SIGTERM`s the daemon once no Claude Code sessions remain.

The daemon itself does the work:

- fsnotify-watches `~/.claude` (minus `~/.claude/cccp`) and `cccp push`es on
  a debounce window (default 500 ms, configurable via `--debounce-ms` /
  `CCCP_DEBOUNCE_MS` / `debounce_ms` in `config.json`).
- Connects to `GET /events` as an SSE subscriber; on each server-side tick
  from another host it runs `cccp pull`.

## Install

```sh
# Make sure cccp is on PATH.
go install github.com/eordano/cccp/cmd/cccp@latest

# Point Claude Code at this plugin directory (one option; the exact install
# method depends on your Claude Code version).
ln -s "$(pwd)/plugin" ~/.claude/plugins/cccp
```

## Config

The binary reuses cccp's four-layer config (defaults < config.json < env <
flag). It needs `--url` and `--token` to talk to the server - put them in
`~/.claude/cccp/config.json`:

```json
{
  "url": "https://sync.example",
  "token": "...",
  "debounce_ms": 500
}
```

## Troubleshooting

- Daemon log: `~/.claude/cccp/secretary.log`
- Status: `cccp watch status`
- Stop: `cccp watch stop`
- Pidfile: `~/.claude/cccp/secretary.pid`
- Session list: `~/.claude/cccp/sessions.json`

## Known limitation

Changes to `settings.json`, hook files, or skills are synced to disk
immediately, but the running `claude-code` process does not reload them.
They take effect on the next session start. This is a Claude Code
limitation; no plugin can fix it.
