package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/cheka"
	"sync/atomic"
	"testing"

	"github.com/eordano/cccp/internal/api"
	"github.com/eordano/cccp/internal/store"
)

const sampleSecret = "ghp_Ab1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0Uv1Wx"

func discardLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func newBackend(t *testing.T, token string) (url string) {
	url, _ = newBackendStore(t, token)
	return url
}

func newBackendStore(t *testing.T, token string) (url string, st *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "srv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	srv := &api.Server{
		Store: s,
		Token: token,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts.URL, s
}

func newClientRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newPushClient(t *testing.T, url, token, root string) *Config {
	t.Helper()
	c, err := New(root, url, token, "claude")
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP = &http.Client{}
	return c
}

func TestPush_OversizedBlobSkipped(t *testing.T) {
	discardLogs(t)
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "CLAUDE.md", "small and clean\n")
	big := filepath.Join(root, "memory", "huge.bin")
	if err := os.MkdirAll(filepath.Dir(big), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxPushBlobBytes + 1); err != nil {
		f.Close()
		t.Skipf("cannot create sparse file: %v", err)
	}
	f.Close()
	c := newPushClient(t, url, "tok", root)

	res, err := Push(context.Background(), c, DefaultAllowlist, 0, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.BlobsUploaded != 1 {
		t.Errorf("BlobsUploaded = %d, want 1 (oversized blob skipped, small one shipped)", res.BlobsUploaded)
	}
	st, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range st.KnownPushedBlobPaths {
		if p == "memory/huge.bin" {
			found = true
		}
	}
	if !found {
		t.Errorf("oversized blob missing from KnownPushedBlobPaths; a later push would tombstone it")
	}
}

func setChunkBytes(t *testing.T, n int) {
	t.Helper()
	prev := LogAppendChunkBytes
	LogAppendChunkBytes = n
	t.Cleanup(func() { LogAppendChunkBytes = prev })
}

func TestPush_LogAppendChunked(t *testing.T) {
	discardLogs(t)
	setChunkBytes(t, 1)
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "history.jsonl",
		`{"n":1}`+"\n"+`{"n":2}`+"\n"+`{"n":3}`+"\n"+`{"n":4}`+"\n")
	c := newPushClient(t, url, "tok", root)

	res, err := Push(context.Background(), c, DefaultAllowlist, 0, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.LogLinesNew != 4 {
		t.Errorf("LogLinesNew = %d, want 4", res.LogLinesNew)
	}
	res2, err := Push(context.Background(), c, DefaultAllowlist, 0, false)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if res2.LogLinesNew != 0 || res2.LogLinesDup != 0 {
		t.Errorf("second push resent lines: new=%d dup=%d, want 0/0", res2.LogLinesNew, res2.LogLinesDup)
	}
}

func TestPush_LogChunkPartialFailureKeepsProgress(t *testing.T) {
	discardLogs(t)
	setChunkBytes(t, 1)
	backendURL := newBackend(t, "tok")
	backend, err := neturl.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	var appendCalls atomic.Int32
	var failing atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(backend)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/log/append" {
			if appendCalls.Add(1) > 1 && failing.Load() {
				http.Error(w, "injected failure", http.StatusInternalServerError)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	failing.Store(true)

	root := newClientRoot(t)
	line1 := `{"n":1}`
	writeFile(t, root, "history.jsonl",
		line1+"\n"+`{"n":2}`+"\n"+`{"n":3}`+"\n")
	c := newPushClient(t, front.URL, "tok", root)

	res, err := Push(context.Background(), c, DefaultAllowlist, 0, false)
	if err == nil {
		t.Fatal("incomplete upload must report failure after saving progress")
	}
	if res.LogLinesNew != 1 {
		t.Errorf("LogLinesNew = %d, want 1 (first chunk landed before failure)", res.LogLinesNew)
	}
	st, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	wantOff := int64(len(line1) + 1)
	if st.LogOffsets["history.jsonl"] != wantOff {
		t.Errorf("LogOffsets = %d, want %d (progress through first chunk persisted)", st.LogOffsets["history.jsonl"], wantOff)
	}

	failing.Store(false)
	res2, err := Push(context.Background(), c, DefaultAllowlist, 0, false)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if res2.LogLinesNew != 2 {
		t.Errorf("second push LogLinesNew = %d, want 2", res2.LogLinesNew)
	}
	if res2.LogLinesDup != 0 {
		t.Errorf("second push LogLinesDup = %d, want 0 (no line resent)", res2.LogLinesDup)
	}
}

func TestPush_CleanSucceeds(t *testing.T) {
	discardLogs(t)
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "CLAUDE.md", "# hello, no secrets here\n")
	writeFile(t, root, "memory/notes.md", "take out the trash")
	c := newPushClient(t, url, "tok", root)

	res, err := Push(context.Background(), c, DefaultAllowlist, 0, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.BlobFiles != 2 {
		t.Errorf("BlobFiles = %d, want 2", res.BlobFiles)
	}
	if res.BlobsUploaded != 2 {
		t.Errorf("BlobsUploaded = %d, want 2", res.BlobsUploaded)
	}
	if res.SecretsFound != 0 {
		t.Errorf("SecretsFound = %d, want 0", res.SecretsFound)
	}
	if res.SecretsRedacted != 0 {
		t.Errorf("SecretsRedacted = %d, want 0 (no redact path hit)", res.SecretsRedacted)
	}
}

func TestPush_BlobWithSecretBlocks(t *testing.T) {
	discardLogs(t)
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "CLAUDE.md", "GITHUB_TOKEN="+sampleSecret+"\n")
	c := newPushClient(t, url, "tok", root)

	_, err := Push(context.Background(), c, DefaultAllowlist, 0, false)
	if err == nil {
		t.Fatal("expected push to fail on secret-bearing blob")
	}
	if !strings.Contains(err.Error(), "cheka") {
		t.Errorf("error should mention cheka: %v", err)
	}
}

func TestPush_ChekaAllowToleratesSecret(t *testing.T) {
	discardLogs(t)
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "CLAUDE.md", "GITHUB_TOKEN="+sampleSecret+"\n")
	c := newPushClient(t, url, "tok", root)

	res, err := Push(context.Background(), c, DefaultAllowlist, 5, false)
	if err != nil {
		t.Fatalf("push with --cheka-allow 5: %v", err)
	}
	if res.SecretsFound == 0 {
		t.Errorf("SecretsFound should be > 0 even when tolerated")
	}
	if res.BlobsUploaded != 1 {
		t.Errorf("BlobsUploaded = %d, want 1 (push should proceed)", res.BlobsUploaded)
	}
}

func TestPush_ChekaAllowNegativeDisables(t *testing.T) {
	discardLogs(t)
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "CLAUDE.md", "GITHUB_TOKEN="+sampleSecret+"\n")
	c := newPushClient(t, url, "tok", root)

	res, err := Push(context.Background(), c, DefaultAllowlist, -1, false)
	if err != nil {
		t.Fatalf("push with --cheka-allow -1: %v", err)
	}
	if res.BlobsUploaded != 1 {
		t.Errorf("BlobsUploaded = %d, want 1", res.BlobsUploaded)
	}
}

func TestPush_RedactLogSecret(t *testing.T) {
	discardLogs(t)
	url := newBackend(t, "tok")
	root := newClientRoot(t)

	writeFile(t, root, "history.jsonl",
		`{"role":"user","text":"my PASSWORD=hunter2_secret123 goes here"}`+"\n"+
			`{"role":"user","text":"another line, clean"}`+"\n")
	c := newPushClient(t, url, "tok", root)

	res, err := Push(context.Background(), c, DefaultAllowlist, 0, true)
	if err != nil {
		t.Fatalf("push with --cheka-redact: %v", err)
	}
	if res.SecretsFound == 0 {
		t.Errorf("SecretsFound should report the scanner's hits")
	}
	if res.SecretsRedacted == 0 {
		t.Errorf("SecretsRedacted = 0; expected log-line redactions to be counted")
	}
	if res.LogLinesNew == 0 {
		t.Errorf("LogLinesNew = 0; push should have sent redacted lines")
	}
}

func TestPush_RedactDoesNotSaveBlobSecrets(t *testing.T) {

	discardLogs(t)
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "CLAUDE.md", "GITHUB_TOKEN="+sampleSecret+"\n")
	writeFile(t, root, "history.jsonl", `{"role":"user","text":"PASSWORD=hunter2_secret"}`+"\n")
	c := newPushClient(t, url, "tok", root)

	_, err := Push(context.Background(), c, DefaultAllowlist, 0, true)
	if err == nil {
		t.Fatal("expected redact-mode push to still fail on blob secret")
	}
	if !strings.Contains(err.Error(), "blobs") {
		t.Errorf("error should mention blobs: %v", err)
	}
}

func TestPush_SecretsRedactedZeroWhenNoRedactPath(t *testing.T) {

	discardLogs(t)
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "history.jsonl", `{"role":"user","text":"PASSWORD=hunter2_secret"}`+"\n")
	c := newPushClient(t, url, "tok", root)

	res, err := Push(context.Background(), c, DefaultAllowlist, 5, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.SecretsRedacted != 0 {
		t.Errorf("SecretsRedacted = %d, want 0 (redact off)", res.SecretsRedacted)
	}
}

func TestScanLogFileSkipsOversizedLines(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big.jsonl")
	big := strings.Repeat("A", MaxScanLine+10)
	os.WriteFile(p, []byte("{\"ok\":1}\n{\"key\":\"AKIA"+big+"\"}\n"), 0o644)
	info, _ := os.Stat(p)
	e := FileEntry{RelPath: "big.jsonl", AbsPath: p, Size: info.Size(), IsLog: true}
	done := make(chan []cheka.Finding, 1)
	go func() { done <- scanLogFile(e, 0) }()
	select {
	case ff := <-done:
		if len(ff) != 0 {
			t.Fatalf("oversized line must not be scanned, got %d findings", len(ff))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scanLogFile spent >5s on an oversized line")
	}
}
