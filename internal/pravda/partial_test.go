package pravda_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eordano/cccp/internal/pravda"
)

func TestHTMXPartialSession(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(session, []byte(`{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := pravda.NewFSStore(dir)
	srv := pravda.NewWatchServer(store)
	go srv.Run(context.Background())
	h := srv.Routes()

	req := httptest.NewRequest("GET", "/session/s.jsonl", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("full: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Fatalf("full page missing doctype")
	}

	req = httptest.NewRequest("GET", "/session/s.jsonl", nil)
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("partial: code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!DOCTYPE") {
		t.Fatalf("partial has doctype")
	}
	if strings.Contains(body, `<aside class="sidebar"`) {
		t.Fatalf("partial has sidebar")
	}
	if strings.Contains(body, "pravda.js") {
		t.Fatalf("partial pulls in scripts")
	}
	if !strings.Contains(body, "timeline-wrap") {
		t.Fatalf("partial missing timeline")
	}
}

func TestHTMXPartialIndex(t *testing.T) {
	dir := t.TempDir()
	store := pravda.NewFSStore(dir)
	srv := pravda.NewWatchServer(store)
	h := srv.Routes()

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!DOCTYPE") {
		t.Fatalf("partial has doctype")
	}
	if !strings.Contains(body, "sidebar") {
		t.Fatalf("missing sidebar: %s", body)
	}
}

func TestCORSPreflight(t *testing.T) {
	dir := t.TempDir()
	store := pravda.NewFSStore(dir)
	srv := pravda.NewWatchServer(store)
	h := srv.Routes()

	req := httptest.NewRequest("OPTIONS", "/session/x.jsonl", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Headers", "HX-Request, Authorization")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("allow-origin=%q", got)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "HX-Request") {
		t.Fatalf("allow-headers=%q", rec.Header().Get("Access-Control-Allow-Headers"))
	}

	req = httptest.NewRequest("OPTIONS", "/session/x.jsonl", nil)
	req.Header.Set("Origin", "https://garden.example.com")
	req.Header.Set("Access-Control-Request-Headers", "HX-Request")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("cross-origin allow-origin=%q, want empty", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("cross-origin allow-credentials=%q, want empty", got)
	}

	req = httptest.NewRequest("GET", "/session/x.jsonl", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("GET cross-origin allow-origin=%q, want empty", got)
	}

	req = httptest.NewRequest("GET", "/session/x.jsonl", nil)
	req.Header.Set("Origin", "http://"+req.Host)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://"+req.Host {
		t.Fatalf("same-origin allow-origin=%q", got)
	}
}
