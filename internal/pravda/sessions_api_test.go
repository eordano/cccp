package pravda

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eordano/cccp/internal/store"
)

func at(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UnixNano()
}

func seedVersions(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	put := func(path string, v int64, body, host, mtime, ingested string) {
		sha := body + "-sha"
		if err := st.PutBlob(ctx, sha, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendVersion(ctx, store.FileVersion{Path: path, Version: v, ContentSHA: &sha, Mode: 0o644,
			MtimeNS: at(mtime), HostID: host, IngestedAt: at(ingested)}); err != nil {
			t.Fatal(err)
		}
	}
	put("projects/-srv-repo/memory/note.md", 1, "old note", "host-a", "2026-09-30T09:00:00Z", "2026-09-30T09:00:01Z")
	put("projects/-srv-repo/memory/note.md", 2, "new note", "host-a", "2026-10-01T10:00:05.500Z", "2026-10-01T10:00:07Z")
	put("projects/-other/memory/elsewhere.md", 1, "other", "host-a", "2026-09-01T00:00:00Z", "2026-09-01T00:00:01Z")
	put("skills/review/SKILL.md", 1, "review v1", "host-b", "2026-09-01T00:00:00Z", "2026-09-01T00:00:01Z")
	put("skills/review/SKILL.md", 2, "review v2", "host-b", "2026-10-01T09:59:00Z", "2026-10-01T10:30:00Z")
	put("skills/review/run.sh", 1, "#!/bin/sh", "host-b", "2026-09-01T00:00:00Z", "2026-09-01T00:00:01Z")
	put("settings.json", 1, "{}", "host-a", "2026-09-01T00:00:00Z", "2026-09-01T00:00:01Z")
}

func newIndexServer(t *testing.T) (*WatchServer, *store.Store) {
	t.Helper()
	st := openIndexStore(t)
	seedSessions(t, st)
	seedVersions(t, st)
	if _, err := (&Indexer{Store: st}).CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return NewWatchServer(NewSQLiteStore(st, nil)), st
}

func getJSON(t *testing.T, h http.Handler, path string, v any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

func TestSessionsAPI(t *testing.T) {
	ws, _ := newIndexServer(t)
	h := ws.Routes()

	var list struct {
		Sessions []sessionJSON
		Total    int
	}
	getJSON(t, h, "/sessions?agent=claude", &list)
	if list.Total != 1 || list.Sessions[0].ID != "aaaa-1111" || list.Sessions[0].Rollup == nil || list.Sessions[0].Rollup.OutputTokens != 350 {
		t.Fatalf("list: %+v", list)
	}

	var d sessionDetail
	getJSON(t, h, "/sessions/aaaa-1111?records=1", &d)
	if d.Session.Path != rootPath || len(d.Children) != 1 || d.Children[0].Path != subPath {
		t.Fatalf("detail: %+v", d.Session)
	}
	bg := map[string]bgFile{}
	for _, f := range d.Background {
		bg[f.Path] = f
	}
	if f := bg["projects/-srv-repo/memory/note.md"]; f.Version != 1 || f.Kind != bgMemory {
		t.Errorf("memory in effect at start must be v1 (v2 was written during the session): %+v", f)
	}
	if _, ok := bg["projects/-other/memory/elsewhere.md"]; ok {
		t.Errorf("another project's memory leaked into the background")
	}
	if f := bg["skills/review/SKILL.md"]; f.Version != 1 || f.Host != "host-b" {
		t.Errorf("host-b's v2 reached the server after the session started: %+v", f)
	}
	if _, ok := bg["skills/review/run.sh"]; ok {
		t.Errorf("only SKILL.md represents a skill")
	}
	if f := bg["/srv/repo/CLAUDE.md"]; f.Synced || f.LoadedAs != "Project" {
		t.Errorf("observed unsynced instructions: %+v", f)
	}
	if f := bg["projects/-srv-repo/memory/MEMORY.md"]; f.Path != "" {
		t.Errorf("an observed file with no synced version must not invent one: %+v", f)
	}

	if d.RecordsTotal == 0 || d.Records[0].ID != "aaaa-1111#1" || d.Records[0].Role != "user" || d.Records[0].SaeID == "" {
		t.Fatalf("records: %+v", d.Records)
	}
	var bare sessionDetail
	getJSON(t, h, "/sessions/aaaa-1111?records=1&detail=0", &bare)
	if bare.Session.Path != rootPath || len(bare.Children) != 0 || bare.Rollup.OutputTokens != 0 || len(bare.Background) != 0 {
		t.Fatalf("detail=0 must skip rollup, children and background: %+v", bare)
	}
	if bare.RecordsTotal != d.RecordsTotal || bare.Records[0].ID != d.Records[0].ID || bare.Records[0].UUID != d.Records[0].UUID {
		t.Fatalf("detail=0 records differ: %+v", bare.Records)
	}
	var write *recordMeta
	for i := range d.Records {
		if d.Records[i].Tool == "Write" {
			write = &d.Records[i]
		}
	}
	if write == nil || write.DurationMs != 1000 || !write.IsError || write.Role != "tool" {
		t.Errorf("tool record: %+v", write)
	}

	var bySID sessionDetail
	getJSON(t, h, "/sessions/child-thread", &bySID)
	if bySID.Parent == nil || bySID.Parent.Path != codexRoot {
		t.Errorf("codex child parent: %+v", bySID.Parent)
	}
}

func TestFileHistoryAttributesWritersAndSessions(t *testing.T) {
	ws, st := newIndexServer(t)
	appendLines(t, st, "host-a", "projects/-srv-repo/bbbb-2222.jsonl",
		jl(map[string]any{"type": "user", "sessionId": "bbbb-2222", "cwd": "/srv/repo", "timestamp": "2026-10-02T08:00:00Z",
			"message": map[string]any{"role": "user", "content": "next day"}}))
	if _, err := (&Indexer{Store: st}).CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := ws.Routes()
	var hist fileHistory
	getJSON(t, h, "/files/projects/-srv-repo/memory/note.md?format=json", &hist)
	if len(hist.Versions) != 2 || hist.Versions[0].Version != 2 {
		t.Fatalf("versions newest first: %+v", hist.Versions)
	}
	v2, v1 := hist.Versions[0], hist.Versions[1]
	if len(v2.WrittenBy) != 1 || v2.WrittenBy[0].Path != rootPath || v2.WrittenBy[0].Tool != "Write" {
		t.Errorf("v2 writer: %+v", v2.WrittenBy)
	}
	if v1.RanWith != 1 || v1.Sessions[0].ID != "aaaa-1111" || v2.RanWith != 1 || v2.Sessions[0].ID != "bbbb-2222" {
		t.Errorf("sessions per version: v1 %+v / v2 %+v", v1, v2)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/projects/-srv-repo/memory/note.md?v=1", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "old note") {
		t.Errorf("version content page: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/files/skills/review/SKILL.md") || !strings.Contains(rec.Body.String(), "-srv-repo/note.md") {
		t.Errorf("catalog page: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index-panel/"+rootPath, nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Background at session start") || !strings.Contains(body, "with 1 sub-sessions") {
		t.Errorf("index panel: %d %s", rec.Code, body)
	}
}

func getLines(t *testing.T, h http.Handler, path string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("GET %s: %d %q %s", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	return strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
}

func TestSessionLines(t *testing.T) {
	ws, _ := newIndexServer(t)
	h := ws.Routes()

	if got, want := getLines(t, h, "/sessions/aaaa-1111?lines=raw"), claudeRootLines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("claude raw lines:\n%v\nwant\n%v", got, want)
	}
	raw := getLines(t, h, "/sessions/parent-thread?lines=raw")
	if strings.Join(raw, "\n") != strings.Join(codexLines("parent-thread", ""), "\n") {
		t.Fatalf("codex raw lines: %v", raw)
	}
	var sawUser bool
	for _, l := range getLines(t, h, "/sessions/parent-thread?lines=converted") {
		var row struct {
			Type    string
			Message struct{ Content any }
		}
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			t.Fatalf("converted line is not JSON: %q", l)
		}
		if row.Type == "user" && strings.Contains(l, "review parent-thread") {
			sawUser = true
		}
	}
	if !sawUser {
		t.Fatalf("converted codex lines carry no Claude-shaped user turn")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/aaaa-1111?lines=parsed", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown lines mode: %d", rec.Code)
	}
}
