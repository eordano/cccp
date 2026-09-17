package pravda_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eordano/cccp/internal/pravda"
)

func writeSessions(t *testing.T, dir string, n int) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("s%02d.jsonl", i))
		line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"prompt %02d"},"timestamp":"2026-01-01T00:00:00Z"}`, i)
		if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndexPagination(t *testing.T) {
	dir := t.TempDir()
	writeSessions(t, dir, 60)
	store := pravda.NewFSStore(dir)
	srv := pravda.NewWatchServer(store)
	go srv.Run(context.Background())
	h := srv.Routes()

	get := func(target string) string {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: code=%d", target, rec.Code)
		}
		return rec.Body.String()
	}

	body := get("/")
	if got := strings.Count(body, `class="session-wrap`); got != 50 {
		t.Fatalf("page 1 rows=%d, want 50", got)
	}
	if !strings.Contains(body, "/session/s60.jsonl") {
		t.Fatalf("page 1 missing newest session")
	}
	if strings.Contains(body, "/session/s01.jsonl") {
		t.Fatalf("page 1 has oldest session")
	}
	if !strings.Contains(body, "1 / 2") || !strings.Contains(body, "?p=2") {
		t.Fatalf("page 1 missing pager: %s", body[len(body)-500:])
	}

	body = get("/?p=2")
	if got := strings.Count(body, `class="session-wrap`); got != 10 {
		t.Fatalf("page 2 rows=%d, want 10", got)
	}
	if !strings.Contains(body, "/session/s01.jsonl") {
		t.Fatalf("page 2 missing oldest session")
	}

	body = get("/?p=99")
	if got := strings.Count(body, `class="session-wrap`); got != 10 {
		t.Fatalf("clamped page rows=%d, want 10", got)
	}

	body = get("/?q=prompt+03")
	if got := strings.Count(body, `class="session-wrap`); got != 1 {
		t.Fatalf("search rows=%d, want 1", got)
	}
	if strings.Contains(body, `class="pager"`) {
		t.Fatalf("single-page result should have no pager")
	}
}

func TestSessionSidebarLocatesCurrentPage(t *testing.T) {
	dir := t.TempDir()
	writeSessions(t, dir, 60)
	store := pravda.NewFSStore(dir)
	srv := pravda.NewWatchServer(store)
	go srv.Run(context.Background())
	h := srv.Routes()

	req := httptest.NewRequest("GET", "/session/s01.jsonl", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/session/s01.jsonl"`) {
		t.Fatalf("sidebar page does not contain current session")
	}
	if !strings.Contains(body, "2 / 2") {
		t.Fatalf("sidebar pager not on page 2")
	}
	if !strings.Contains(body, `href="/session/s01.jsonl?p=1"`) {
		t.Fatalf("pager prev link should stay on current session URL")
	}
}
