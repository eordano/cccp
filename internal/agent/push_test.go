package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	c, err := New(root, url, token)
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP = &http.Client{}
	return c
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
