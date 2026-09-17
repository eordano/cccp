package pravda

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

type lineStore struct {
	lines     []string
	calls     []int64
	facts     map[string]cachedFacts
	saved     int
	headOpens int
}

func (m *lineStore) ListSessions(context.Context) ([]SessionMeta, error) {
	return []SessionMeta{{Path: "projects/p/s.jsonl", LineCount: len(m.lines), ModTime: time.Now()}}, nil
}
func (m *lineStore) OpenSession(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(strings.Join(m.lines, "\n"))), nil
}
func (m *lineStore) OpenSessionHead(ctx context.Context, p string, _ int) (io.ReadCloser, error) {
	m.headOpens++
	return m.OpenSession(ctx, p)
}
func (m *lineStore) Subscribe(context.Context) <-chan string { return nil }
func (m *lineStore) SessionLines(_ context.Context, _ string, after int64) ([][]byte, int64, error) {
	m.calls = append(m.calls, after)
	var out [][]byte
	var cursor int64
	for i, l := range m.lines {
		seen := int64(i + 1)
		if seen > after {
			out = append(out, []byte(l))
			cursor = seen
		}
	}
	return out, cursor, nil
}
func (m *lineStore) LoadFacts(context.Context) (map[string]cachedFacts, error) { return m.facts, nil }
func (m *lineStore) SaveFacts(_ context.Context, p string, n int, f *sessionFacts) error {
	m.saved++
	m.facts[p] = cachedFacts{facts: f, lines: n}
	return nil
}

func TestEvents_IncrementalFetchUsesCursor(t *testing.T) {
	st := &lineStore{lines: []string{
		`{"type":"user","uuid":"1","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"2","message":{"role":"assistant","content":[{"type":"text","text":"yo"}]}}`,
	}, facts: map[string]cachedFacts{}}
	srv := NewWatchServer(st)
	ctx := context.Background()
	rel := "projects/p/s.jsonl"

	ev, err := srv.events(ctx, rel, srv.pathVersion(rel))
	if err != nil || len(ev) != 2 {
		t.Fatalf("first load: %d events, err %v", len(ev), err)
	}
	st.lines = append(st.lines, `{"type":"user","uuid":"3","message":{"role":"user","content":"more"}}`)
	srv.onLogEvent(rel)
	ev, err = srv.events(ctx, rel, srv.pathVersion(rel))
	if err != nil || len(ev) != 3 {
		t.Fatalf("after append: %d events, err %v", len(ev), err)
	}
	if len(st.calls) != 2 || st.calls[0] != 0 || st.calls[1] != 2 {
		t.Fatalf("SessionLines called with %v, want [0 2]", st.calls)
	}
	if ev, _ = srv.events(ctx, rel, srv.pathVersion(rel)); len(ev) != 3 || len(st.calls) != 2 {
		t.Fatalf("cached read refetched: calls %v", st.calls)
	}
}

func TestCollectSessions_FactsPersistAcrossRestart(t *testing.T) {
	st := &lineStore{facts: map[string]cachedFacts{}}
	for i := 0; i < headLines+1; i++ {
		st.lines = append(st.lines, `{"type":"user","uuid":"u","cwd":"/w","sessionId":"sid","message":{"role":"user","content":"a title"}}`)
	}
	ctx := context.Background()
	if _, err := NewWatchServer(st).collectSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if st.saved != 1 || st.headOpens != 1 {
		t.Fatalf("cold run: saved %d facts, opened %d heads; want 1 and 1", st.saved, st.headOpens)
	}
	groups, err := NewWatchServer(st).collectSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.headOpens != 1 {
		t.Fatalf("warm run re-read the session head (%d opens)", st.headOpens)
	}
	if len(groups) != 1 || groups[0].Session.Facts == nil || groups[0].Session.Facts.CWD != "/w" {
		t.Fatalf("warm run lost facts: %+v", groups)
	}
}
