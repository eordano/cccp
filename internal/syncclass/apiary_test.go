package syncclass

import "testing"

func TestClassifyApiary(t *testing.T) {
	for _, tc := range []struct {
		wire string
		mode uint32
		want Class
	}{
		{"agent/apiary/coder/memories/x.md", 0o644, ClassKnowledge},
		{"agent/apiary/global/prompt-notes/stop-looping.md", 0o644, ClassKnowledge},
		{"agent/apiary/coder/skills/x/SKILL.md", 0o644, ClassKnowledge},
		{"agent/apiary/global/router/labels/b1.md", 0o644, ClassKnowledge},
		{"agent/apiary/global/router/promotions/ck1.md", 0o644, ClassKnowledge},
		{"agent/apiary/coder/notes.txt", 0o644, ClassKnowledge},
		{"agent/apiary/coder/skills/x/run.sh", 0o644, ClassPrivileged},
		{"agent/apiary/coder/skills/x/run.sh", 0o755, ClassPrivileged},
		{"agent/apiary/coder/skills/x/SKILL.md", 0o755, ClassPrivileged},
		{"agent/apiary/coder/memories/x.txt", 0o700, ClassPrivileged},
		{"agent/apiary/config.yaml", 0o644, ClassPrivileged},
		{"agent/apiary/coder/settings.json", 0o644, ClassPrivileged},
		{"agent/apiary/coder/hooks/pre.py", 0o644, ClassPrivileged},
		{"agent/apiary/coder/memories/x.markdown", 0o644, ClassPrivileged},
		{"agent/apiary/", 0o644, ClassPrivileged},
	} {
		if got := Classify(tc.wire, tc.mode); got != tc.want {
			t.Errorf("Classify(%q, %o) = %v, want %v", tc.wire, tc.mode, got, tc.want)
		}
	}
}

func TestApiaryLeavesOtherAgentsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		wire string
		mode uint32
		want Class
	}{
		{"settings.json", 0o600, ClassPrivileged},
		{"CLAUDE.md", 0o644, ClassKnowledge},
		{"skills/rcon/cheat.sh", 0o644, ClassPrivileged},
		{"projects/-srv-repo/abc.jsonl", 0o644, ClassTranscript},
		{"agent/codex/AGENTS.md", 0o644, ClassKnowledge},
		{"agent/codex/config.toml", 0o644, ClassPrivileged},
		{"agent/codex/sessions/2026/10/r.jsonl", 0o644, ClassTranscript},
		{"agent/hermes/memories/MEMORY.md", 0o644, ClassKnowledge},
		{"agent/hermes/config.yaml", 0o600, ClassPrivileged},
		{"agent/hermes/transcripts/t.jsonl", 0o644, ClassTranscript},
		{"agent/opencode/transcripts/t.jsonl", 0o644, ClassTranscript},
		{"agent/opencode/plugin/x.js", 0o644, ClassPrivileged},
		{"agent/unknown/notes.md", 0o644, ClassPrivileged},
	} {
		if got := Classify(tc.wire, tc.mode); got != tc.want {
			t.Errorf("Classify(%q, %o) = %v, want %v", tc.wire, tc.mode, got, tc.want)
		}
	}
	if len(byAgent) != 5 {
		t.Errorf("byAgent has %d classifiers, want claude/codex/hermes/opencode/apiary", len(byAgent))
	}
}
