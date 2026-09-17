package pravda

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

type memStore struct{ lines map[string]string }

func (m memStore) ListSessions(context.Context) ([]SessionMeta, error) { return nil, nil }
func (m memStore) OpenSession(_ context.Context, p string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(m.lines[p])), nil
}
func (m memStore) Subscribe(context.Context) <-chan string { return nil }

func TestAgentOf(t *testing.T) {
	cases := map[string]string{
		"projects/x/y.jsonl":               "",
		"agent/codex/sessions/a.jsonl":     "codex",
		"agent/hermes/transcripts/b.jsonl": "hermes",
		"agent/":                           "",
		"agents/reviewer.md":               "",
	}
	for in, want := range cases {
		if got := agentOf(in); got != want {
			t.Errorf("agentOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertingStore(t *testing.T) {
	RegisterConverter("testagent", func(line []byte) [][]byte {
		if bytes.Contains(line, []byte("drop")) {
			return nil
		}
		return [][]byte{bytes.ToUpper(line), []byte(`{"dup":true}`)}
	})
	defer delete(converters, "testagent")
	st := withConverters(memStore{lines: map[string]string{
		"agent/testagent/s.jsonl": "{\"a\":1}\n{\"drop\":1}\n\n{\"b\":2}\n",
		"projects/p/s.jsonl":      "{\"a\":1}\n",
	}})
	if _, ok := st.(sessionHeadOpener); ok {
		t.Fatal("wrapper must not advertise OpenSessionHead when the inner store lacks it")
	}
	rc, err := st.OpenSession(context.Background(), "agent/testagent/s.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	want := "{\"A\":1}\n{\"dup\":true}\n{\"B\":2}\n{\"dup\":true}\n"
	if string(got) != want {
		t.Fatalf("converted = %q, want %q", got, want)
	}
	rc, _ = st.OpenSession(context.Background(), "projects/p/s.jsonl")
	got, _ = io.ReadAll(rc)
	if string(got) != "{\"a\":1}\n" {
		t.Fatalf("claude path must pass through, got %q", got)
	}
}

func TestSessionFilter(t *testing.T) {
	RegisterSessionFilter("filt", func(rel string) bool { return strings.HasPrefix(rel, "sessions/") })
	defer delete(sessionFilters, "filt")
	if !isListedSession("projects/p/s.jsonl") || !isListedSession("agent/other/x.jsonl") {
		t.Fatal("paths without a filter must be listed")
	}
	if !isListedSession("agent/filt/sessions/a.jsonl") || isListedSession("agent/filt/session_index.jsonl") {
		t.Fatal("filter not applied")
	}
	if !isListedSession("agent/codex/sessions/2026/09/08/r.jsonl") || isListedSession("agent/codex/session_index.jsonl") {
		t.Fatal("codex filter not registered")
	}
}
