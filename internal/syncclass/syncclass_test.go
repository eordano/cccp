package syncclass

import "testing"

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		wire string
		mode uint32
		want Class
	}{
		{"hooks/no-pipe-tail.sh", 0o755, ClassPrivileged},
		{"hooks/readme.md", 0o644, ClassPrivileged},
		{"settings.json", 0o600, ClassPrivileged},
		{"settings.local.json", 0o600, ClassPrivileged},
		{"plugins/installed_plugins.json", 0o600, ClassPrivileged},
		{".mcp.json", 0o644, ClassPrivileged},
		{"unknown-new-dir/x.md", 0o644, ClassPrivileged},
		{"CLAUDE.md", 0o644, ClassKnowledge},
		{"CLAUDE.md", 0o755, ClassPrivileged},
		{"memory/MEMORY.md", 0o644, ClassKnowledge},
		{"projects/-srv-repo/memory/note.md", 0o644, ClassKnowledge},
		{"projects/-srv-repo/memory/run.sh", 0o644, ClassPrivileged},
		{"skills/rcon/SKILL.md", 0o644, ClassKnowledge},
		{"skills/rcon/cheat.sh", 0o644, ClassPrivileged},
		{"skills/rcon/notes.txt", 0o755, ClassPrivileged},
		{"commands/review.md", 0o644, ClassKnowledge},
		{"agents/reviewer.md", 0o644, ClassKnowledge},
		{"projects/-srv-repo/abc.jsonl", 0o644, ClassTranscript},
		{"history.jsonl", 0o600, ClassTranscript},
		{"plans/p.md", 0o644, ClassTranscript},
		{"file-history/s/x@v1", 0o644, ClassTranscript},
		{"agent/codex/config.toml", 0o644, ClassPrivileged},
		{"agent/codex/hooks.json", 0o644, ClassPrivileged},
		{"agent/codex/rules/default.rules", 0o644, ClassPrivileged},
		{"agent/codex/AGENTS.md", 0o644, ClassKnowledge},
		{"agent/codex/skills/x/SKILL.md", 0o644, ClassKnowledge},
		{"agent/codex/sessions/2026/10/r.jsonl", 0o644, ClassTranscript},
		{"agent/hermes/config.yaml", 0o600, ClassPrivileged},
		{"agent/hermes/memories/MEMORY.md", 0o644, ClassKnowledge},
		{"agent/hermes/SOUL.md", 0o644, ClassKnowledge},
		{"agent/hermes/transcripts/t.jsonl", 0o644, ClassTranscript},
		{"agent/opencode/transcripts/t.jsonl", 0o644, ClassTranscript},
		{"agent/opencode/plugin/x.js", 0o644, ClassPrivileged},
		{"agent/unknown/transcripts/t.jsonl", 0o644, ClassPrivileged},
		{"agent/", 0o644, ClassPrivileged},
		{"agent/hermes-buzz-fizz/transcripts/t.jsonl", 0o644, ClassTranscript},
		{"agent/hermes-buzz-fizz/memories/MEMORY.md", 0o644, ClassKnowledge},
		{"agent/hermes-buzz-fizz/config.yaml", 0o600, ClassPrivileged},
		{"agent/opencode-buzz-comb/transcripts/t.jsonl", 0o644, ClassTranscript},
		{"agent/claude-x/projects/-srv/a.jsonl", 0o644, ClassPrivileged},
		{"agent/claude-x/memory/MEMORY.md", 0o644, ClassPrivileged},
		{"agent/hermes-/transcripts/t.jsonl", 0o644, ClassPrivileged},
		{"agent/hermes-Buzz/transcripts/t.jsonl", 0o644, ClassPrivileged},
		{"agent/nope-x/transcripts/t.jsonl", 0o644, ClassPrivileged},
	} {
		if got := Classify(tc.wire, tc.mode); got != tc.want {
			t.Errorf("Classify(%q, %o) = %v, want %v", tc.wire, tc.mode, got, tc.want)
		}
	}
}

func TestKind(t *testing.T) {
	for name, want := range map[string]string{
		"claude":           "claude",
		"hermes":           "hermes",
		"hermes-buzz-fizz": "hermes",
		"opencode-decent":  "opencode",
		"claude-x":         "",
		"hermes-":          "",
		"-x":               "",
		"hermes-Fizz":      "",
		"hermes-a/b":       "",
	} {
		if got := Kind(name); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestReviewable(t *testing.T) {
	for _, tc := range []struct {
		wire     string
		mode     uint32
		own      bool
		reviewed bool
	}{
		{"memory/MEMORY.md", 0o644, false, true},
		{"CLAUDE.md", 0o644, false, true},
		{"settings.json", 0o600, false, false},
		{"agent/hermes/memories/MEMORY.md", 0o644, false, true},
		{"agent/hermes/SOUL.md", 0o644, false, true},
		{"agent/apiary/coder/memories/x.md", 0o644, false, true},
		{"agent/apiary/global/router/promotions/c.md", 0o644, false, true},
		{"agent/codex/AGENTS.md", 0o644, false, true},
		{"agent/hermes-buzz-fizz/memories/MEMORY.md", 0o644, true, false},
		{"agent/hermes-buzz-fizz/memories/USER.md", 0o644, true, false},
		{"agent/hermes-buzz-fizz/SOUL.md", 0o644, true, false},
		{"agent/hermes-buzz-fizz/transcripts/s.jsonl", 0o644, true, false},
		{"agent/codex-lab/AGENTS.md", 0o644, true, false},
		{"agent/hermes-Fizz/memories/MEMORY.md", 0o644, false, false},
		{"agent/claude-x/CLAUDE.md", 0o644, false, false},
	} {
		if got := OwnSpace(tc.wire); got != tc.own {
			t.Errorf("OwnSpace(%q) = %v, want %v", tc.wire, got, tc.own)
		}
		if got := Reviewable(tc.wire, tc.mode); got != tc.reviewed {
			t.Errorf("Reviewable(%q) = %v, want %v", tc.wire, got, tc.reviewed)
		}
	}
}
