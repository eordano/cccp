package secretary

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eordano/cccp/internal/agent"
	"github.com/eordano/cccp/internal/api"
	"github.com/eordano/cccp/internal/store"
)

func testSetup(t *testing.T) (url string, rootA, rootB string, a, b *agent.Config) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "srv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	srv := &api.Server{
		Store: s,
		Token: "tok",
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	rootA = t.TempDir()
	rootB = t.TempDir()
	a, err = agent.New(rootA, ts.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	b, err = agent.New(rootB, ts.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	return ts.URL, rootA, rootB, a, b
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	bs, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(bs)
}

func startSerializer(t *testing.T, opt Options, localDirty *atomic.Bool) chan<- kind {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	triggers := make(chan kind, 64)
	go runSerializer(ctx, triggers, localDirty, opt)
	return triggers
}

func TestSerializer_PullSkippedWhilePushFails(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, rootA, rootB, a, b := testSetup(t)
	ctx := context.Background()

	write(t, rootA, "memory/MEMORY.md", "server content")
	if _, err := agent.Push(ctx, a, nil, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Pull(ctx, b, false); err != nil {
		t.Fatal(err)
	}

	write(t, rootB, "memory/MEMORY.md", "unpushed local edit")
	write(t, rootB, "settings.json", `{"env":{"GITHUB_TOKEN":"ghp_Ab1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0Uv1Wx"}}`)

	var localDirty atomic.Bool
	localDirty.Store(true)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	triggers := startSerializer(t, Options{Agent: b, Root: rootB, ChekaAllow: 0, Log: log}, &localDirty)

	triggers <- kindPull
	time.Sleep(700 * time.Millisecond)

	triggers <- kindPull
	time.Sleep(700 * time.Millisecond)

	if got := read(t, rootB, "memory/MEMORY.md"); got != "unpushed local edit" {
		t.Errorf("pull ran despite failing push and reverted local edit; content = %q", got)
	}
}

func TestSerializer_PullTriggerRunsPushFirstWhenDirty(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, rootA, rootB, a, b := testSetup(t)
	ctx := context.Background()

	write(t, rootA, "memory/MEMORY.md", "server content")
	if _, err := agent.Push(ctx, a, nil, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Pull(ctx, b, false); err != nil {
		t.Fatal(err)
	}

	write(t, rootB, "memory/MEMORY.md", "edit saved during debounce window")

	var localDirty atomic.Bool
	localDirty.Store(true)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	triggers := startSerializer(t, Options{Agent: b, Root: rootB, ChekaAllow: 0, Log: log}, &localDirty)

	triggers <- kindPull

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := agent.Pull(ctx, a, false); err == nil {
			if read(t, rootA, "memory/MEMORY.md") == "edit saved during debounce window" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got := read(t, rootB, "memory/MEMORY.md"); got != "edit saved during debounce window" {
		t.Errorf("SSE-triggered pull overwrote fresh local edit; content = %q", got)
	}
	if got := read(t, rootA, "memory/MEMORY.md"); got != "edit saved during debounce window" {
		t.Errorf("edit was not pushed before pull; host A has %q", got)
	}
}

// No filesystem event follows the failure: reconciliation alone must recover.
func TestSerializer_PushOnlyRetriesIdleUpload(t *testing.T) {
	var calls atomic.Int32
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/log/append" {
			t.Errorf("push-only requested %s", r.URL.Path)
			http.Error(w, "unexpected request", 500)
			return
		}
		if calls.Add(1) == 1 {
			http.Error(w, "temporary outage", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"new":1,"dup":0}`)
		select {
		case delivered <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	root := t.TempDir()
	write(t, root, "history.jsonl", "{\"message\":\"last session line\"}\n")
	client, err := agent.New(root, server.URL, "tok", "claude")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	triggers := make(chan kind, 1)
	triggers <- kindPush
	var dirty atomic.Bool
	go func() {
		defer close(done)
		runSerializer(ctx, triggers, &dirty, Options{
			Agent: client, Root: root, PushOnly: true,
			ReconcileInterval: 20 * time.Millisecond,
			Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("idle upload was never retried")
	}
}
