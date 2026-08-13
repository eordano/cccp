package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

func TestPull_EmptySnapshotDoesNotDeleteLocalFiles(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url := newBackend(t, "tok")
	rootA := newClientRoot(t)
	rootB := newClientRoot(t)
	writeFile(t, rootA, "memory/notes.md", "keep me")
	writeFile(t, rootA, "CLAUDE.md", "keep me too")
	a := newPushClient(t, url, "tok", rootA)
	if _, err := Push(ctx, a, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}
	b := newPushClient(t, url, "tok", rootB)
	if _, err := Pull(ctx, b, false); err != nil {
		t.Fatal(err)
	}
	if !exists(rootB, "memory/notes.md") || !exists(rootB, "CLAUDE.md") {
		t.Fatal("pull did not materialize files on B")
	}

	freshURL := newBackend(t, "tok")
	b2 := newPushClient(t, freshURL, "tok", rootB)
	res, err := Pull(ctx, b2, false)
	if err != nil {
		t.Fatalf("pull against empty server: %v", err)
	}
	if res.TarRemoved != 0 {
		t.Errorf("TarRemoved = %d, want 0", res.TarRemoved)
	}
	if !exists(rootB, "memory/notes.md") || !exists(rootB, "CLAUDE.md") {
		t.Error("empty server snapshot deleted previously pulled local files")
	}

	pres, err := Push(ctx, b2, DefaultAllowlist, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if pres.BlobsDeleted != 0 {
		t.Errorf("BlobsDeleted = %d, want 0 (no tombstone cascade)", pres.BlobsDeleted)
	}
}

func TestPull_TombstoneKeepsLocallyModifiedFile(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url := newBackend(t, "tok")
	rootA := newClientRoot(t)
	rootB := newClientRoot(t)
	writeFile(t, rootA, "skills/foo.md", "original")
	writeFile(t, rootA, "skills/keep.md", "stays")
	a := newPushClient(t, url, "tok", rootA)
	if _, err := Push(ctx, a, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}
	b := newPushClient(t, url, "tok", rootB)
	if _, err := Pull(ctx, b, false); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(rootA, "skills/foo.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, a, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}

	writeFile(t, rootB, "skills/foo.md", "local edit, never pushed")
	res, err := Pull(ctx, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.TarRemoved != 0 {
		t.Errorf("TarRemoved = %d, want 0", res.TarRemoved)
	}
	if got := readFile(t, rootB, "skills/foo.md"); got != "local edit, never pushed" {
		t.Errorf("locally modified file was destroyed by tombstone; content = %q", got)
	}
}

func TestPull_TombstoneRemovesUnmodifiedFile(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url := newBackend(t, "tok")
	rootA := newClientRoot(t)
	rootB := newClientRoot(t)
	writeFile(t, rootA, "skills/foo.md", "original")
	writeFile(t, rootA, "skills/keep.md", "stays")
	a := newPushClient(t, url, "tok", rootA)
	if _, err := Push(ctx, a, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}
	b := newPushClient(t, url, "tok", rootB)
	if _, err := Pull(ctx, b, false); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(rootA, "skills/foo.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, a, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}

	res, err := Pull(ctx, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.TarRemoved != 1 {
		t.Errorf("TarRemoved = %d, want 1", res.TarRemoved)
	}
	if exists(rootB, "skills/foo.md") {
		t.Error("unmodified tombstoned file should have been removed")
	}
	if !exists(rootB, "skills/keep.md") {
		t.Error("live file should remain")
	}
}

func TestPull_FullPreservesUnpushedLogLinesAndResetsOffset(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url := newBackend(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "history.jsonl", `{"n":1}`+"\n"+`{"n":2}`+"\n")
	c := newPushClient(t, url, "tok", root)
	if _, err := Push(ctx, c, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(filepath.Join(root, "history.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"n":3,"local":true}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := Pull(ctx, c, true); err != nil {
		t.Fatal(err)
	}
	content := readFile(t, root, "history.jsonl")
	if !strings.Contains(content, `{"n":3,"local":true}`) {
		t.Fatalf("pull --full destroyed unpushed local line; content:\n%s", content)
	}
	if !strings.Contains(content, `{"n":1}`) || !strings.Contains(content, `{"n":2}`) {
		t.Fatalf("pull --full lost server lines; content:\n%s", content)
	}

	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	wantOff := int64(len(`{"n":1}` + "\n" + `{"n":2}` + "\n"))
	if got := state.LogOffsets["history.jsonl"]; got != wantOff {
		t.Errorf("LogOffsets = %d, want %d (start of local-only tail)", got, wantOff)
	}

	res, err := Push(ctx, c, DefaultAllowlist, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.LogLinesNew != 1 || res.LogLinesDup != 0 {
		t.Errorf("post-full push: new=%d dup=%d, want new=1 dup=0", res.LogLinesNew, res.LogLinesDup)
	}
}

func TestPush_OversizedLogLineSkippedOffsetAdvances(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url, st := newBackendStore(t, "tok")
	root := newClientRoot(t)
	huge := `{"blob":"` + strings.Repeat("x", 5*1024*1024) + `"}`
	writeFile(t, root, "history.jsonl", `{"n":1}`+"\n"+huge+"\n"+`{"n":3}`+"\n")
	c := newPushClient(t, url, "tok", root)

	res, err := Push(ctx, c, DefaultAllowlist, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.LogLinesNew != 2 {
		t.Errorf("LogLinesNew = %d, want 2 (line after oversized one must sync)", res.LogLinesNew)
	}
	lines, err := st.LogContents(ctx, "history.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Errorf("server has %d lines, want 2", len(lines))
	}

	res2, err := Push(ctx, c, DefaultAllowlist, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	if res2.LogLinesNew != 0 || res2.LogLinesDup != 0 {
		t.Errorf("second push: new=%d dup=%d, want 0/0 (offset must advance past oversized line)",
			res2.LogLinesNew, res2.LogLinesDup)
	}
}

func TestPush_DanglingSymlinkSkippedWithoutTombstone(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url := newBackend(t, "tok")
	rootA := newClientRoot(t)
	writeFile(t, rootA, "skills/a.md", "aaa")
	writeFile(t, rootA, "skills/b.md", "bbb")
	a := newPushClient(t, url, "tok", rootA)
	if _, err := Push(ctx, a, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(rootA, "skills/b.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/target", filepath.Join(rootA, "skills/b.md")); err != nil {
		t.Fatal(err)
	}
	res, err := Push(ctx, a, DefaultAllowlist, 0, false)
	if err != nil {
		t.Fatalf("push with dangling symlink must not abort: %v", err)
	}
	if res.BlobsDeleted != 0 {
		t.Errorf("BlobsDeleted = %d, want 0 (dangling symlink is not a deletion)", res.BlobsDeleted)
	}

	rootB := newClientRoot(t)
	b := newPushClient(t, url, "tok", rootB)
	if _, err := Pull(ctx, b, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, rootB, "skills/b.md"); got != "bbb" {
		t.Errorf("server content for b.md = %q, want %q", got, "bbb")
	}
}

func TestPush_StaleManifestDoesNotResurrectTombstone(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url := newBackend(t, "tok")
	rootA := newClientRoot(t)
	rootB := newClientRoot(t)
	writeFile(t, rootA, "commands/x.md", "delete me")
	writeFile(t, rootA, "memory/keep.md", "v1")
	a := newPushClient(t, url, "tok", rootA)
	if _, err := Push(ctx, a, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}
	b := newPushClient(t, url, "tok", rootB)
	if _, err := Pull(ctx, b, false); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(rootA, "commands/x.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Push(ctx, a, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}

	writeFile(t, rootB, "memory/keep.md", "v2 from B")
	if _, err := Push(ctx, b, DefaultAllowlist, 0, false); err != nil {
		t.Fatal(err)
	}

	if _, err := Pull(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if exists(rootA, "commands/x.md") {
		t.Error("deleted file resurrected by stale push from host that had not pulled the tombstone")
	}
	if got := readFile(t, rootA, "memory/keep.md"); got != "v2 from B" {
		t.Errorf("keep.md = %q, want %q (live edits still propagate)", got, "v2 from B")
	}
}
