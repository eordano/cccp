package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWirePrefix(t *testing.T) {
	if got := WirePrefix("claude"); got != "" {
		t.Fatalf("claude prefix = %q", got)
	}
	if got := WirePrefix(""); got != "" {
		t.Fatalf("empty prefix = %q", got)
	}
	if got := WirePrefix("codex"); got != "agent/codex/" {
		t.Fatalf("codex prefix = %q", got)
	}
}

func TestWalkPrefix(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "sessions", "a", "x.jsonl"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "top.md"), []byte("hi"), 0o644)
	entries, _, err := Walk(root, "agent/codex/", []string{"sessions/", "top.md"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.RelPath] = e.IsLog
		if _, err := os.Stat(e.AbsPath); err != nil {
			t.Fatalf("abs path %s: %v", e.AbsPath, err)
		}
	}
	if !got["agent/codex/sessions/a/x.jsonl"] {
		t.Fatalf("log entry missing or not IsLog: %v", got)
	}
	if isLog, ok := got["agent/codex/top.md"]; !ok || isLog {
		t.Fatalf("blob entry wrong: %v", got)
	}
}

func TestLocalRel(t *testing.T) {
	claude := &Config{Agent: "claude"}
	if rel, ok := claude.localRel("projects/x/y.jsonl"); !ok || rel != "projects/x/y.jsonl" {
		t.Fatalf("claude own path: %q %v", rel, ok)
	}
	if _, ok := claude.localRel("agent/codex/sessions/x.jsonl"); ok {
		t.Fatal("claude must ignore other agents' paths")
	}
	if rel, ok := claude.localRel("agents/reviewer.md"); !ok || rel != "agents/reviewer.md" {
		t.Fatalf("claude agents/ dir must still sync: %q %v", rel, ok)
	}
	codex := &Config{Agent: "codex", Prefix: WirePrefix("codex")}
	if rel, ok := codex.localRel("agent/codex/sessions/x.jsonl"); !ok || rel != "sessions/x.jsonl" {
		t.Fatalf("codex own path: %q %v", rel, ok)
	}
	if _, ok := codex.localRel("projects/x/y.jsonl"); ok {
		t.Fatal("codex must ignore claude paths")
	}
	if _, ok := codex.localRel("agent/hermes/transcripts/x.jsonl"); ok {
		t.Fatal("codex must ignore hermes paths")
	}
}

func TestNewRejectsUnknownAgent(t *testing.T) {
	for _, name := range []string{"nope", "nope-x", "claude-x", "hermes-", "hermes-Fizz"} {
		if _, err := New(t.TempDir(), "http://x", "tok", name); err == nil {
			t.Fatalf("%q: expected unknown agent error", name)
		}
	}
	t.Setenv("HOME", t.TempDir())
	own, err := New(t.TempDir(), "http://x", "tok", "hermes-buzz-fizz")
	if err != nil {
		t.Fatal(err)
	}
	if own.Agent != "hermes-buzz-fizz" || own.Prefix != "agent/hermes-buzz-fizz/" {
		t.Fatalf("own space: %q %q", own.Agent, own.Prefix)
	}
	if got := own.DefaultAllowlist(); len(got) != len(HermesAllowlist) || got[0] != HermesAllowlist[0] {
		t.Fatalf("own space allowlist = %v, want the hermes profile's", got)
	}
	if rel, ok := own.localRel("agent/hermes-buzz-fizz/transcripts/x.jsonl"); !ok || rel != "transcripts/x.jsonl" {
		t.Fatalf("own space path: %q %v", rel, ok)
	}
	if _, ok := own.localRel("agent/hermes/transcripts/x.jsonl"); ok {
		t.Fatal("an own space must ignore the shared hermes space")
	}
	c, err := New(t.TempDir(), "http://x", "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != DefaultAgent || c.Prefix != "" {
		t.Fatalf("default agent: %q %q", c.Agent, c.Prefix)
	}
}
