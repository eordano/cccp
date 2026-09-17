package pravda

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeFixtures(t *testing.T) {
	cases := []struct {
		name     string
		wantMin  int
		wantKind string
	}{
		{"sample1.jsonl", 20, KindTool},
		{"sample2.jsonl", 20, KindTool},
		{"sample3.jsonl", 50, KindTool},
		{"sample4-files.jsonl", 20, KindAttachment},
		{"synthetic-task.jsonl", 3, KindTool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("fixtures", tc.name))
			if err != nil {
				t.Skipf("fixture missing: %v", err)
			}
			defer f.Close()
			events, err := ParseJSONL(f)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			sess := Normalize(events, nil)
			if len(sess.Entries) < tc.wantMin {
				t.Errorf("got %d entries, want >= %d", len(sess.Entries), tc.wantMin)
			}
			found := false
			for _, e := range sess.Entries {
				if e.Kind == tc.wantKind {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("no entry with kind=%q", tc.wantKind)
			}
			if sess.Metadata == "" {
				t.Error("empty metadata")
			}
			t.Logf("%s: %d entries · %s", tc.name, len(sess.Entries), sess.Metadata)
		})
	}
}

func TestSubAgentFile(t *testing.T) {
	path := "/Users/EXAMPLE/.claude/projects/-Users-EXAMPLE-projects-claude-watch/0b20586f-374a-4bda-aca5-e4c97d62260d/subagents/agent-a1fbf49600c89e5b5.jsonl"
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("agent fixture missing: %v", err)
	}
	defer f.Close()
	events, err := ParseJSONL(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sess := Normalize(events, nil)
	if len(sess.Entries) == 0 {
		t.Fatal("expected entries for agent file, got 0 (sidechain filter regression?)")
	}
	t.Logf("agent file: %d entries · %s", len(sess.Entries), sess.Metadata)
}

func TestAgentSpawnDetection(t *testing.T) {
	path := "/Users/EXAMPLE/.claude/projects/-Users-EXAMPLE-projects-claude-watch/0b20586f-374a-4bda-aca5-e4c97d62260d.jsonl"
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("parent session missing: %v", err)
	}
	defer f.Close()
	events, err := ParseJSONL(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sess := Normalize(events, nil)
	agents := 0
	for _, e := range sess.Entries {
		if e.Kind == KindTool && (e.Name == "Agent" || e.Name == "Task") && e.AgentRef != nil {
			agents++
		}
	}
	if agents == 0 {
		t.Fatal("expected at least one Agent tool_use with AgentRef populated")
	}
	t.Logf("%d agent spawns with AgentRef", agents)
}
