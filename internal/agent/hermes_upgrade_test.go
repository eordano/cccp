package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/eordano/cccp/internal/store"
)

func asOldHermes(t *testing.T) func() {
	t.Helper()
	cur := profiles["hermes"]
	old := cur
	old.Allowlist = []string{"transcripts/", "memories/", "SOUL.md", "config.yaml"}
	old.Exclude = nil
	profiles["hermes"] = old
	restore := func() { profiles["hermes"] = cur }
	t.Cleanup(restore)
	return restore
}

func newHermes(t *testing.T, root, url string) *Config {
	t.Helper()
	c, err := New(root, url, "tok", "hermes")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pushHermes(t *testing.T, c *Config) *PushResult {
	t.Helper()
	res, err := Push(context.Background(), c, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func latest(t *testing.T, st *store.Store, path string) *store.FileVersion {
	t.Helper()
	fv, err := st.LatestVersion(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return fv
}

var droppedHermes = []string{"agent/hermes/config.yaml", "agent/hermes/transcripts/.backfill.json"}

func TestPush_HermesUpgradeStopsSyncingWithoutTombstones(t *testing.T) {
	discardLogs(t)
	t.Setenv("HOME", t.TempDir())
	url, st := newBackendStore(t, "tok")
	root := newClientRoot(t)
	writeFile(t, root, "config.yaml", "model: a\n")
	writeFile(t, root, "SOUL.md", "soul\n")
	writeFile(t, root, "transcripts/.backfill.json", `{"sessions":{}}`)
	writeFile(t, root, "transcripts/s1.jsonl", "{\"type\":\"user\",\"n\":1}\n")
	c := newHermes(t, root, url)

	restore := asOldHermes(t)
	if res := pushHermes(t, c); res.BlobsCommitted != 3 {
		t.Fatalf("old binary committed %d blobs, want 3", res.BlobsCommitted)
	}
	restore()

	res := pushHermes(t, c)
	if res.BlobsDeleted != 0 || res.BlobsCommitted != 0 {
		t.Fatalf("first push of the new binary: %+v, want no deletion and nothing new", res)
	}
	for _, p := range droppedHermes {
		if fv := latest(t, st, p); fv == nil || fv.ContentSHA == nil || fv.Version != 1 {
			t.Errorf("%s: latest %+v, want the old binary's live v1 untouched", p, fv)
		}
	}
	state, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range droppedHermes {
		if slices.Contains(state.KnownPushedBlobPaths, p) {
			t.Errorf("state still knows %s", p)
		}
		if _, ok := state.BlobCache[p]; ok {
			t.Errorf("state still caches %s", p)
		}
	}

	writeFile(t, root, "config.yaml", "model: b\n")
	writeFile(t, root, "transcripts/.backfill.json", `{"sessions":{"x":{}}}`)
	writeFile(t, root, "transcripts/.backfill.json.tmp", `{}`)
	writeFile(t, root, "transcripts/.backfill.lock", "")
	if res := pushHermes(t, c); res.BlobsCommitted != 0 || res.BlobsDeleted != 0 {
		t.Errorf("edits to unsynced files reached the server: %+v", res)
	}
	for _, p := range []string{"config.yaml", "transcripts/.backfill.json"} {
		if err := os.Remove(filepath.Join(root, p)); err != nil {
			t.Fatal(err)
		}
	}
	if res := pushHermes(t, c); res.BlobsDeleted != 0 {
		t.Errorf("removing unsynced files tombstoned %d paths", res.BlobsDeleted)
	}
	for _, p := range append(droppedHermes, "agent/hermes/transcripts/.backfill.json.tmp", "agent/hermes/transcripts/.backfill.lock") {
		fv := latest(t, st, p)
		switch {
		case p == "agent/hermes/config.yaml" || p == "agent/hermes/transcripts/.backfill.json":
			if fv == nil || fv.ContentSHA == nil || fv.Version != 1 {
				t.Errorf("%s: latest %+v, want v1 live", p, fv)
			}
		case fv != nil:
			t.Errorf("%s reached the server: %+v", p, fv)
		}
	}
	if fv := latest(t, st, "agent/hermes/SOUL.md"); fv == nil || fv.ContentSHA == nil {
		t.Errorf("SOUL.md: %+v, want it still synced", fv)
	}
}

func TestHermesMixedVersionsShareAgentHermes(t *testing.T) {
	discardLogs(t)
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	url, st := newBackendStore(t, "tok")
	rootA, rootB := newClientRoot(t), newClientRoot(t)
	for root, who := range map[string]string{rootA: "a", rootB: "b"} {
		writeFile(t, root, "config.yaml", "host: "+who+"\n")
		writeFile(t, root, "transcripts/.backfill.json", `{"from":"`+who+`"}`)
		writeFile(t, root, "transcripts/"+who+".jsonl", "{\"type\":\"user\",\"who\":\""+who+"\"}\n")
		writeFile(t, root, "transcripts/"+who+".json", "a transcript-class blob keeps the snapshot non-empty")
	}
	a, b := newHermes(t, rootA, url), newHermes(t, rootB, url)

	restore := asOldHermes(t)
	pushHermes(t, a)
	pushHermes(t, b)
	mustPull(t, ctx, a, false)
	restore()
	pulledStamp := readFile(t, rootA, "transcripts/.backfill.json")
	if pulledStamp != `{"from":"b"}` {
		t.Fatalf("old pull left A's stamp at %q; the setup expects B's", pulledStamp)
	}

	if res := pushHermes(t, a); res.BlobsDeleted != 0 {
		t.Fatalf("upgraded A tombstoned %d paths", res.BlobsDeleted)
	}
	old := asOldHermes(t)
	writeFile(t, rootB, "config.yaml", "host: b2\n")
	writeFile(t, rootB, "transcripts/.backfill.json", `{"from":"b2"}`)
	if res := pushHermes(t, b); res.BlobsCommitted != 2 {
		t.Fatalf("old B committed %d blobs, want 2", res.BlobsCommitted)
	}
	old()

	res, err := Pull(ctx, a, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.TarRemoved != 0 {
		t.Errorf("upgraded A removed %d files on pull", res.TarRemoved)
	}
	if got := readFile(t, rootA, "transcripts/.backfill.json"); got != pulledStamp {
		t.Errorf("upgraded A's stamp = %q after pull, want it left alone at %q", got, pulledStamp)
	}
	if got := readFile(t, rootA, "transcripts/b.jsonl"); got == "" {
		t.Error("transcripts stopped syncing")
	}
	if res := pushHermes(t, a); res.BlobsDeleted != 0 || res.BlobsCommitted != 0 {
		t.Errorf("upgraded A after pull: %+v", res)
	}
	for _, p := range droppedHermes {
		fv := latest(t, st, p)
		if fv == nil || fv.ContentSHA == nil || fv.HostID != b.HostID {
			t.Errorf("%s: latest %+v, want old B's live version", p, fv)
		}
	}
}
