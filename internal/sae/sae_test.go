package sae

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/eordano/cccp/internal/store"
)

func writeSafetensors(t *testing.T, path string, idx []int64, vals []float32, n, k int, scale []float32) {
	t.Helper()
	var data bytes.Buffer
	header := map[string]any{}
	off := 0
	add := func(name, dtype string, shape []int, size int, write func()) {
		header[name] = map[string]any{"dtype": dtype, "shape": shape, "data_offsets": []int{off, off + size}}
		write()
		off += size
	}
	add("indices", "I64", []int{n, k}, 8*len(idx), func() {
		for _, v := range idx {
			binary.Write(&data, binary.LittleEndian, v)
		}
	})
	add("values", "F32", []int{n, k}, 4*len(vals), func() {
		for _, v := range vals {
			binary.Write(&data, binary.LittleEndian, v)
		}
	})
	add("scale", "F32", []int{len(scale)}, 4*len(scale), func() {
		for _, v := range scale {
			binary.Write(&data, binary.LittleEndian, v)
		}
	})
	hb, _ := json.Marshal(header)
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint64(len(hb)))
	out.Write(hb)
	out.Write(data.Bytes())
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRun lays out an nv-sae run directory with n records over width
// features; record i activates features (i, i+1, i+2) mod width.
func writeRun(t *testing.T, dir, enc string, n, width int) {
	t.Helper()
	k := 3
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	res := fmt.Sprintf(`{"encoder_sha256":%q,"config":{"width":%d,"top_k":%d}}`, enc, width, k)
	os.WriteFile(filepath.Join(dir, "result.json"), []byte(res), 0o644)
	var recs bytes.Buffer
	idx := make([]int64, 0, n*k)
	vals := make([]float32, 0, n*k)
	for i := 0; i < n; i++ {
		kind := "user"
		if i%2 == 1 {
			kind = "assistant"
		}
		fmt.Fprintf(&recs, `{"id":"id%03d","kind":%q,"agent":"claude","host":"h","session":"s","time":"2026-09-01T00:00:00Z","source":"/m/p%d.jsonl","line":%d,"text":"record %d text"}`+"\n", i, kind, i%3, i, i)
		for j := 0; j < k; j++ {
			idx = append(idx, int64((i+j)%width))
			vals = append(vals, float32(1+j))
		}
	}
	os.WriteFile(filepath.Join(dir, "records.jsonl"), recs.Bytes(), 0o644)
	scale := make([]float32, width)
	for i := range scale {
		scale[i] = 4
	}
	writeSafetensors(t, filepath.Join(dir, "scored.safetensors"), idx, vals, n, k, scale)
}

func openDB(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func bruteSimilar(recs map[string]Record, target []Activation, kind string) []string {
	tv := map[int]float64{}
	var tn float64
	for _, a := range target {
		tv[a.Feature] = float64(a.Pct)
		tn += float64(a.Pct) * float64(a.Pct)
	}
	type sc struct {
		id string
		s  float64
	}
	var out []sc
	for id, r := range recs {
		if kind != "" && r.Kind != kind {
			continue
		}
		var dot, n float64
		for _, a := range r.Features {
			n += float64(a.Pct) * float64(a.Pct)
			if t, ok := tv[a.Feature]; ok {
				dot += t * float64(a.Pct)
			}
		}
		if dot > 0 {
			out = append(out, sc{id, dot / math.Sqrt(tn*n)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].s != out[j].s {
			return out[i].s > out[j].s
		}
		return out[i].id < out[j].id
	})
	ids := make([]string, len(out))
	for i, o := range out {
		ids[i] = o.id
	}
	return ids
}

func TestImportSimilarCountsPrune(t *testing.T) {
	ctx := context.Background()
	st := openDB(t)
	dir := t.TempDir()
	writeRun(t, filepath.Join(dir, "a"), "encA", 40, 8)
	n, err := ImportWith(ctx, st.DB(), filepath.Join(dir, "a"), ImportOptions{StripPrefix: "/m/", Layer: 19, Label: "daily-20260801", Promote: true})
	if err != nil || n != 40 {
		t.Fatalf("import: %d %v", n, err)
	}
	ix, err := New(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]Record{}
	for i := 0; i < 40; i++ {
		r, err := ix.Get(ctx, "encA", fmt.Sprintf("id%03d", i))
		if err != nil || r == nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if r.Path != fmt.Sprintf("p%d.jsonl", i%3) {
			t.Fatalf("strip prefix: %q", r.Path)
		}
		all[r.ID] = *r
	}
	target := all["id005"].Features
	if len(target) != 3 {
		t.Fatalf("features: %+v", target)
	}
	for _, kind := range []string{"", "user", "assistant"} {
		got, err := ix.Similar(ctx, "encA", target, kind, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := bruteSimilar(all, target, kind)
		if len(got) != len(want) {
			t.Fatalf("kind %q: %d matches, want %d", kind, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("kind %q rank %d: %s want %s", kind, i, got[i].ID, want[i])
			}
		}
		top, _ := ix.Similar(ctx, "encA", target, kind, 5)
		if len(top) != 5 || top[0].ID != want[0] {
			t.Fatalf("kind %q limit: %+v", kind, top)
		}
	}
	counts, total, err := ix.FeatureCounts(ctx, "encA")
	if err != nil || total != 40 {
		t.Fatalf("counts: %d %v", total, err)
	}
	for f := 0; f < 8; f++ {
		if counts[f] != 15 {
			t.Fatalf("feature %d count %d", f, counts[f])
		}
	}

	// A second, older daily that is not current gets pruned; the current one stays.
	writeRun(t, filepath.Join(dir, "b"), "encB", 10, 8)
	if _, err := ImportWith(ctx, st.DB(), filepath.Join(dir, "b"), ImportOptions{Layer: 19, Label: "daily-20260701"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE sae_encoders SET imported_at=? WHERE encoder='encB'`, time.Now().Add(-40*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	stale, err := ix.StaleDaily(ctx, 30*24*time.Hour)
	if err != nil || len(stale) != 1 || stale[0].Label != "daily-20260701" || stale[0].Records != 10 {
		t.Fatalf("stale: %+v %v", stale, err)
	}
	if encs, _ := ix.Encoders(ctx); len(encs) != 2 {
		t.Fatalf("listing must not delete: %+v", encs)
	}
	if err := ix.DeleteEncoder(ctx, stale[0].Encoder); err != nil {
		t.Fatal(err)
	}
	encs, _ := ix.Encoders(ctx)
	if len(encs) != 1 || encs[0].Encoder != "encA" || encs[0].Records != 40 {
		t.Fatalf("after prune: %+v", encs)
	}
	var left int
	st.DB().QueryRow(`SELECT count(*) FROM sae_record_features WHERE encoder='encB'`).Scan(&left)
	if left != 0 {
		t.Fatalf("feature rows left: %d", left)
	}
}

func TestBackfillFeatureIndex(t *testing.T) {
	ctx := context.Background()
	st := openDB(t)
	dir := t.TempDir()
	writeRun(t, filepath.Join(dir, "a"), "encA", 20, 8)
	if _, err := ImportWith(ctx, st.DB(), filepath.Join(dir, "a"), ImportOptions{Layer: 19}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM sae_record_features; UPDATE sae_records SET norm=0`); err != nil {
		t.Fatal(err)
	}
	ix, err := New(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	st.DB().QueryRow(`SELECT count(*) FROM sae_record_features`).Scan(&rows)
	if rows != 60 {
		t.Fatalf("backfilled %d rows", rows)
	}
	r, _ := ix.Get(ctx, "encA", "id000")
	got, err := ix.Similar(ctx, "encA", r.Features, "", 1)
	if err != nil || len(got) != 1 || got[0].ID != "id000" || math.Abs(got[0].Score-1) > 1e-9 {
		t.Fatalf("self match: %+v %v", got, err)
	}
}
