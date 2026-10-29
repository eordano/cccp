package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func bump(t *testing.T, root, rel string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(d)
	if err := os.Chtimes(filepath.Join(root, rel), when, when); err != nil {
		t.Fatal(err)
	}
}

func TestIngest_UnchangedResendIsNotAVersion(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url, st := newBackendStore(t, "tok")
	rootA, rootB := newClientRoot(t), newClientRoot(t)
	writeFile(t, rootA, "memory/m.md", "studio's memory\n")
	writeFile(t, rootB, "memory/m.md", "giga's memory\n")
	a := newPushClient(t, url, "tok", rootA)
	b := newPushClient(t, url, "tok", rootB)
	if a.HostID == b.HostID {
		t.Fatal("setup: the two homes share a host id")
	}
	newest := func() int64 {
		t.Helper()
		fv := latest(t, st, "memory/m.md")
		if fv == nil {
			return 0
		}
		return fv.Version
	}

	for range 5 {
		mustPush(t, ctx, a)
		mustPush(t, ctx, b)
	}
	if v := newest(); v != 2 {
		t.Fatalf("two homes re-sending unchanged files made %d versions, want 2", v)
	}
	if n := len(reviewList(t, url, false)); n != 2 {
		t.Errorf("review queue holds %d items, want 2", n)
	}

	writeFile(t, rootA, "memory/m.md", "studio's memory, edited\n")
	bump(t, rootA, "memory/m.md", time.Second)
	mustPush(t, ctx, a)
	writeFile(t, rootA, "memory/m.md", "studio's memory\n")
	bump(t, rootA, "memory/m.md", 2*time.Second)
	mustPush(t, ctx, a)
	mustPush(t, ctx, b)
	if v := newest(); v != 4 {
		t.Fatalf("an edit and its revert on A: newest v%d, want v4", v)
	}
	if fv := latest(t, st, "memory/m.md"); fv.HostID != a.HostID {
		t.Errorf("B's unchanged re-send became current: %+v", fv)
	}

	writeFile(t, rootB, "memory/m.md", "giga's memory\n")
	bump(t, rootB, "memory/m.md", 3*time.Second)
	mustPush(t, ctx, b)
	if v := newest(); v != 4 {
		t.Errorf("B rewrote the content it last sent and made v%d, want none", v)
	}
}

func TestIngest_ResendDoesNotBlockRecreationAfterTombstone(t *testing.T) {
	discardLogs(t)
	ctx := context.Background()
	url, st := newBackendStore(t, "tok")
	rootA, rootB := newClientRoot(t), newClientRoot(t)
	writeFile(t, rootA, "plans/p.md", "plan\n")
	writeFile(t, rootB, "plans/p.md", "other plan\n")
	a := newPushClient(t, url, "tok", rootA)
	b := newPushClient(t, url, "tok", rootB)
	mustPush(t, ctx, a)
	mustPush(t, ctx, b)
	if err := os.Remove(filepath.Join(rootB, "plans/p.md")); err != nil {
		t.Fatal(err)
	}
	mustPush(t, ctx, b)
	mustPush(t, ctx, a)
	if fv := latest(t, st, "plans/p.md"); fv.ContentSHA != nil {
		t.Fatalf("A's stale copy resurrected the deleted file: %+v", fv)
	}
	bump(t, rootA, "plans/p.md", time.Minute)
	mustPush(t, ctx, a)
	if fv := latest(t, st, "plans/p.md"); fv.ContentSHA == nil || fv.HostID != a.HostID {
		t.Errorf("A re-creating its file after the tombstone: latest %+v", fv)
	}
}
