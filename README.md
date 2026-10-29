# cccp

Cloud Code Config Provider. A tool to sync `~/.claude` across machines.

Claude Code doesn't have a built-in cloud sync. For working across hosts and
VMs, I built this to synchronize memories, project notes, hooks, skills,
settings, and conversational history. Three particularities about the sync
mechanism: files ending in `.jsonl` (like conversation logs) use append-only
dedup, deleted files are tracked through tombstones, and what an agent writes is
treated as untrusted input (see [Review gate](#review-gate)): hooks, settings
and other executable configuration are archived but never applied on another
host, and memory, `CLAUDE.md` and skill prose reach other hosts only once a
human approves the version. The binary uses as few dependencies as possible.

## Quickstart

```sh
# the server
export CCCP_TOKEN=$(openssl rand -hex 32)
cccp serve --port 12110

# a client (on any machine)
export CCCP_URL=https://your-server:12110
export CCCP_TOKEN=<same token>
cccp pull --full    # bootstrap: grab everything
cccp push           # ship local state
cccp watch          # incremental pushes on change

# anywhere
cccp explore        # only open logs ui on .claude folder (no sync)
```

### SAE conversation map

When an SAE dictionary is imported, Pravda shows feature activations in compact
tracks below the conversation. Search the feature selector to toggle multiple
features; colors stay consistent across tracks and message details. Select a
message in the map to inspect its values and jump to the corresponding event.
Overview and heatmap views provide alternative ways to explore the same data.

Open **Settings → SAE display and status** to choose a dictionary, configure
default features and colors, adjust the display threshold, and inspect index
availability. Display preferences are saved in this browser per dictionary;
individual conversations can override the defaults. Missing records are shown
separately from imported records whose activations fall below the threshold.

Activation percentages are rounded, scaled values clipped at 100. They are
not probabilities or confidence scores and do not have to add up to 100.
The same dictionary also analyzes thinking, tool inputs and text results, and
other events containing readable text. These features describe the capture
model's representation of that text at one layer, regardless of which agent
produced it. Each dictionary belongs to exactly one capture model and layer
(shown next to it in pravda): activations of different models are not
comparable, so a dictionary only scores captures from its own model. Image
results and events containing only timing or other metadata are excluded.

With on-demand scoring configured, viewing an unscored text event queues its SAE
execution and shows progress while the conversation remains usable. Results
are stored per message and dictionary and reused on later visits. Interrupted
jobs resume after restart; temporary failures retry with capped backoff until
the service recovers. Permanent errors stop retrying and show as failed.
Inputs up to 1 MiB are supported. When the capture token limit is exceeded,
the text is split without truncation and chunk activations are averaged by
token count before SAE scoring. Chunks have independent model context; this
is not equivalent to processing the entire text in one context window.
Status covers
the imported index and these jobs; it does not monitor external training.

To enable scoring, pass `--sae-capture-url http://HOST:PORT` and
`--sae-models-dir /path/to/models` to `cccp serve` (NixOS:
`services.cccp.saeCaptureURL` and `saeModelsDir`). Each dictionary needs
`models/ENCODER_SHA256/sae.safetensors` and its original `scored.safetensors`
or a `scale.safetensors` containing the original `scale` tensor. The worker
verifies the weights' SHA256 and preserves the dictionary's original scaling;
new dictionaries need their matching files installed before on-demand scoring
is available. The worker calls the capture service's `POST /v1/capture` with
the dictionary's model and layer (falling back to `/v1/laguna/capture` on an
engine without that route) and checks that the reply names the same model,
layer and the dictionary's hidden size. A dictionary whose model the engine
does not serve, an engine that cannot capture (HTTP 501), or a model or
hidden-size mismatch fails the job permanently with a message naming the
dictionary's model and what the engine answered.

`cccp sae import` and `cccp sae push` read the capture model and layer from
the run's `result.json` (`model`, `layer`). `--model` and `--layer` only fill
in a legacy `result.json` that names neither (default
`poolside/Laguna-XS-2.1-NVFP4`, layer 19, which is what every dictionary
trained before the field existed used); a flag that disagrees with
`result.json` is an error. On first start after upgrade, existing
dictionaries are recorded as that Laguna model.

## Review gate

An agent can write anything under its own home, including from inside a
sandbox, so every synced path has a class (`internal/syncclass`):

| class | claude | codex | hermes | opencode | on pull |
|-------|--------|-------|--------|----------|---------|
| privileged | `hooks/`, `settings.json`, `settings.local.json`, `plugins/`, `.mcp.json`, any script or executable (also inside `skills/`), anything unclassified | `config.toml`, `hooks.json`, `rules/`, skill scripts | `config.yaml` (no longer synced; earlier versions stay archived) | everything but transcripts | never applied; archived on the server for forensics. Source of truth: the repo / home-manager. |
| knowledge | `CLAUDE.md`, `memory/`, `projects/*/memory/`, prose (`.md`, `.txt`) in `skills/`, `commands/`, `agents/` | `AGENTS.md`, prose in `skills/`, `memories/` | `SOUL.md`, `memories/` | - | applied only once a human approves that version |
| transcript | `projects/` sessions, `history.jsonl`, `file-history/`, `paste-cache/`, `plans/`, `tasks/`, `todos/` | `sessions/`, `archived_sessions/`, `history.jsonl`, `session_index.jsonl` | `transcripts/` | `transcripts/` | synced as before |

A push stores every version with its host, agent and the host's active session
ids. Knowledge pushed into an agent's own space (`agent/<agent>-<space>/`, for
example a Buzz agent's `agent/hermes-buzz-fizz/memories/`) is archived instead:
stored and visible in pravda, never queued and not approvable, because only the
home that writes a space ever pulls it. Review the queue at `/review` on the
server (same login as pravda) or:

```sh
cccp review list                     # pending knowledge proposals
cccp review show memory/notes.md 7   # provenance, pre-check, diff, history
cccp review approve memory/notes.md 7
cccp review reject CLAUDE.md 3
cccp review approve memory/notes.md 5   # an older version: rollback
```

The host that wrote a pending file keeps its local copy; pull never overwrites
a local edit with an approved version, never replaces a symlink (home-manager
files), and never writes an executable bit. `cccp serve --review-precheck CMD`
runs CMD on each proposal (content on stdin) and shows its verdict in the queue;
it never approves anything.

When a server first runs a gate-aware binary it stamps `gate_since`. Versions
pushed before it are `legacy`: they stay where they already are, are never
served to gate-aware clients unless someone approves them, and older clients
keep getting the snapshot frozen at that moment (so they neither receive new
agent-written files nor delete what they already pulled).

## Other agents

The same binary syncs Codex, Hermes and OpenCode. `--agent NAME` (or
`CCCP_AGENT`, or `"agent"` in the config file) picks a profile: the agent's
own state directory becomes `--dir`, the profile's allowlist decides what ships,
and everything lands on the server under `agent/<name>/`, so the sessions show
up in pravda alongside Claude's. Several homes of one agent that should not
share a space (one per Buzz agent, say) name their own after a dash:
`--agent hermes-buzz-fizz` is the hermes profile under `agent/hermes-buzz-fizz/`.

```sh
# per-agent config: ~/.config/cccp/<agent>.json, else the agent's own dir
# (~/.codex/cccp/config.json, ~/.hermes/cccp/config.json, ...)
cccp watch --agent codex
cccp push  --agent hermes
```

Codex already writes JSONL rollouts; Hermes and OpenCode need the plugin from
`plugin/<agent>/`, which writes a JSONL transcript per session and starts the
daemon via `cccp watch hook-start --agent <agent>` (see each plugin's README).

## The Apparatus

One binary to rule them all. The party does not tolerate unnecessary
proliferation of executables.

`cccp serve` runs the central server. `cccp pull` retrieves files. This tries
to be as unopinionated as possible; the rest of the tool introduces more
nuanced choices.

cheka (the secret scanner) is the Secret Police, built into `cccp push`. It
detects API keys, tokens, passwords, private keys, and other secrets before
they leave the motherland. Findings are local-only. Pulls are intentionally
unscanned - what the central server sends is, by definition, correct and
trustworthy. The trust boundary, plainly: content pushed to the central
server is trusted and never re-scanned on pull, so a compromised server
could deliver unscanned content to every client.

`cccp watch` is the Secretary General. A tireless daemon that watches
`~/.claude` for changes and pushes them, subscribes to the server's SSE
stream and pulls on remote changes. It can run standalone or be driven
by Claude Code's session hooks. No orphan processes shall haunt the
workers' machines.

`cccp explore` is the Voice of Truth. A session viewer that renders
Claude Code conversations as rich timelines - tool calls, responses,
thinking blocks, metadata - all server-rendered with HTMX. No React
(bourgeois decadence), no build step (unnecessary complexity is the
enemy of the people). Also ships embedded in `cccp serve`.

## The Secretary General

The Secretary watches `~/.claude` via fsnotify, pushes on a 500ms
debounce window, and pulls immediately when the server broadcasts an
SSE event from another host. Three goroutines coordinate through
channels - because concurrent access to `state.json` is
counter-revolutionary.

Unlike a manual `cccp push`, the Secretary defaults to `--cheka-redact`:
secrets found in log lines are replaced with `[REDACTED:kind]` and the
push proceeds - a daemon that silently stops syncing serves nobody.
Set `cheka_redact: false` (or `--cheka-redact=false`) to restore
blocking. Blob files containing secrets still block either way.

You can run the daemon directly:

```sh
cccp watch --url https://your-server:12110 --token "$CCCP_TOKEN"
```

### Wiring as a Claude Code plugin

Add to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [{
      "matcher": "",
      "hooks": [{ "type": "command", "command": "cccp watch hook-start" }]
    }],
    "SessionEnd": [{
      "matcher": "",
      "hooks": [{ "type": "command", "command": "cccp watch hook-end" }]
    }]
  }
}
```

`hook-start` spawns the daemon and records the session. `hook-end`
drops it. When the session count hits zero: SIGTERM, clean exit, no
questions asked.

### Manual control

```sh
cccp watch status   # PID, session count, daemon state
cccp watch          # blocking foreground (for interrogation)
cccp watch stop     # SIGTERM the daemon
```

## Pravda (the web transcript viewer) - the Voice of Truth

Browse Claude Code sessions as rich timelines from any machine in the
fleet. Dark and light themes, responsive layout, live updates via
WebSocket.

```sh
cccp explore                        # ~/.claude/projects/ by default
cccp explore /path/to/projects      # custom directory
cccp explore -p 8080                # custom port (default 9123)
```

Also ships embedded in `cccp serve`, served at the root URL, gated by
cookie auth (same token as the sync API). The people deserve
transparency.

### The session index

`cccp serve` keeps a per-session index derived from the raw log: agent,
host, user, cwd, git branch and commit, models, start and end, turns,
model calls, tool calls, errors, tokens, reported cost, parent and
sub-agent links, title and first prompt. It folds new lines as they
arrive and never rewrites the log; a store that was never indexed (or was
indexed by an older binary) is rebuilt in the background on start, or by
hand with `cccp index rebuild` (resumable, safe next to a running serve).

Each session page gets a **Session record** panel: the rolled-up cost and
tokens of the session and all its sub-agents and child threads, and the
**background** it started with -- every memory file, `CLAUDE.md` /
`AGENTS.md` / `SOUL.md`, skill, command, agent definition, hook and
settings file at the version that was in effect on that host at session
start, plus instruction files the agent reported loading that cccp does
not sync. `/files` lists the synced background files; `/files/{path}`
shows each version, its review state, the session whose tool call wrote it
and the sessions that started with it.

## Configuration

The party has determined that configuration requires four layers of
bureaucracy, each capable of overriding the one below it:

1. Built-in defaults
2. Config file (`$XDG_CONFIG_HOME/cccp/<agent>.json` when it exists, else
   `$dir/cccp/config.json`)
3. Environment variables (`CCCP_URL`, `CCCP_TOKEN`, `CCCP_DIR`, ...)
4. CLI flags (`--url`, `--token`, `--dir`, ...)

Prefer `token_file` (`--token-file`, `CCCP_TOKEN_FILE`) over `token`, and keep
both the token and the config outside the agent's directory: anything under
`~/.claude` is readable and writable by the agent, sandboxed or not. A config
the agent can edit can point the daemon, and its token, at another server.

An explicit empty string in a higher layer overrides a non-empty value
below it. False and unset are not the same - a philosophical truth that
transcends mere programming.

The allowlist (which paths under `~/.claude` to sync) lives in the
config file only. Ordered arrays are awkward to quote in shells, and
this is a set-once-per-host knob. The default syncs: `agents/`,
`commands/`, `file-history/`, `hooks/`, `memory/`, `paste-cache/`,
`plans/`, `projects/`, `skills/`, `tasks/`, `CLAUDE.md`,
`settings.json`, `history.jsonl`.

See `man cccp` for the full flag and environment variable reference.

## HTTP API

All endpoints require `Authorization: Bearer $TOKEN`.

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/ingest` | Submit file manifest (NDJSON). Server responds with SHAs it needs. |
| `PUT` | `/blob/{sha}` | Upload a content-addressed blob. |
| `GET` | `/current` | Snapshot as a tar archive. With `X-Cccp-Gate: 1`: transcripts plus approved knowledge versions; without: transcripts plus everything else frozen at `gate_since`. |
| `POST` | `/log/append` | Append jsonl lines (deduped by content hash). |
| `GET` | `/log/since?since={ns}` | Fetch log lines newer than cursor. |
| `GET` | `/events` | SSE stream - ticks on every push, tagged with host ID. |
| `GET` | `/review/api/list[?all=1]` | Pending knowledge proposals (all: every non-transcript version since the gate). |
| `GET` | `/review/api/show?path=&version=` | One version: provenance, pre-check, diff, history, decisions. |
| `POST` | `/review/api/decide` | `{"path","version","decision":"approved"\|"rejected","note","reviewer"}`. |
| `GET` | `/sessions` | Indexed root sessions, newest first, each with its descendant rollup. Filters: `agent`, `host`, `project`, `q`, `parent` (children of a path), `all=1` (sub-agents too), `since` (RFC3339 or ns), `limit` (<= 500), `offset`. |
| `GET` | `/sessions/{key}` | One session by log path, index id or transcript session id: row, parent, children, rollup, background at start, listed/invoked skills. `?records=1[&offset=&limit=]` adds every normalized record: `id` (`<session id>#<ordinal>`), role, kind, tool, duration, error flag, model, uuid and SAE record id. `?detail=0` leaves out the rollup, children, parent, background and skills (the row and the records stay): background at start reads the file-version history, which made one detail 40-60 s on production (2026-10-29). `?lines=raw` returns the transcript's lines instead, as stored (NDJSON, ordered by suggested time); `?lines=converted` returns them in the Claude Code shape for agents with a converter (Codex, Hermes, opencode). |
| `GET` | `/files[/{path}]?format=json` | Background files, or one file's versions with writer and session attribution. |
| `GET` | `/healthz` | Returns `ok`. No auth required. |

Three tokens. The full token reaches everything. The push token
(`--push-token`) reaches only `/ingest`, `/blob/{sha}`, `/log/append` and
`/sae/import`, and can never read. The read token (`--read-token-file`,
`CCCP_READ_TOKEN_FILE`, config `read_token_file`) reaches only `GET`/`HEAD`
on an allowlist of read routes: `/current`, `/log/since`, `/events`,
`/sae/feedback`, the review queue views (`/review`, `/review/show`,
`/review/api/list`, `/review/api/show`), and the pravda pages (`/`,
`/session/*`, `/sessions*`, `/files*`, `/index-panel/*`, `/subagent/*`,
`/sae/*` read views, `/ws*`). Every write, every review decision (API, form
and `cccp review approve|reject`) and the login form answer 403, and so does
any route not on the list. It is for automation that must read sessions but
must never approve its own review-gate proposals.

## Installation

```sh
# from source
go install github.com/eordano/cccp/cmd/cccp@latest

# nix
nix profile install github:eordano/cccp

# nixos module
{
  inputs.cccp.url = "github:eordano/cccp";
  # ...
  services.cccp = {
    enable = true;
    package = inputs.cccp.packages.${pkgs.system}.default;
    tokenFile = "/run/secrets/cccp-token";
  };
}
```

The Nix flake ships the binary, man page, and shell completions
(bash, zsh, fish). The NixOS module runs the server under systemd with
`LoadCredential` for the token.

## Not in scope

Telemetry ingest, eviction, PII scrubbing, built-in TLS. TLS is
terminated upstream by Caddy, nginx, or Tailscale Funnel. The party
does not concern itself with the bourgeois complexities of certificate
management.
