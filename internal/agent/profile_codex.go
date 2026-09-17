package agent

import (
	"os"
	"path/filepath"
)

var CodexAllowlist = []string{
	"sessions/",
	"archived_sessions/",
	"rules/",
	"skills/",
	"AGENTS.md",
	"config.toml",
	"history.jsonl",
	"hooks.json",
	"session_index.jsonl",
}

func codexDir(home string) string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".codex")
}

func init() {
	RegisterProfile(Profile{Name: "codex", DefaultDir: codexDir, Allowlist: CodexAllowlist})
}
