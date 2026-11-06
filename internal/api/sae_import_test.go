package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eordano/cccp/internal/sae"
)

func tinySafetensors(t *testing.T, n, k, width int) []byte {
	t.Helper()
	var data bytes.Buffer
	header := map[string]any{}
	off := 0
	put := func(name, dtype string, shape []int, size int, w func()) {
		header[name] = map[string]any{"dtype": dtype, "shape": shape, "data_offsets": []int{off, off + size}}
		w()
		off += size
	}
	put("indices", "I64", []int{n, k}, 8*n*k, func() {
		for i := 0; i < n*k; i++ {
			binary.Write(&data, binary.LittleEndian, int64(i%width))
		}
	})
	put("values", "F32", []int{n, k}, 4*n*k, func() {
		for i := 0; i < n*k; i++ {
			binary.Write(&data, binary.LittleEndian, float32(2))
		}
	})
	put("scale", "F32", []int{width}, 4*width, func() {
		for i := 0; i < width; i++ {
			binary.Write(&data, binary.LittleEndian, float32(4))
		}
	})
	hb, _ := json.Marshal(header)
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint64(len(hb)))
	out.Write(hb)
	out.Write(data.Bytes())
	return out.Bytes()
}

func TestSaeImportGzipStoresPage(t *testing.T) {
	s, _, _ := newTestServer(t, "tok")
	s.PushToken = "push"
	s.SaeDir = t.TempDir()
	h := s.Routes()

	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	mw := multipart.NewWriter(zw)
	files := map[string][]byte{
		"result.json":        []byte(`{"encoder_sha256":"encT","config":{"width":4,"top_k":2}}`),
		"records.jsonl":      []byte(`{"id":"a","kind":"user","agent":"claude","host":"h","session":"s","time":"t","source":"/m/x.jsonl","line":1,"text":"hello"}` + "\n" + `{"id":"b","kind":"assistant","agent":"claude","host":"h","session":"s","time":"t","source":"/m/x.jsonl","line":2,"text":"world"}` + "\n"),
		"scored.safetensors": tinySafetensors(t, 2, 2, 4),
		"sessions.html":      []byte("<html><body>" + string(bytes.Repeat([]byte("stance "), 2000)) + "</body></html>"),
	}
	for name, content := range files {
		part, _ := mw.CreateFormFile(name, name)
		part.Write(content)
	}
	mw.WriteField("label", "daily-20260901")
	mw.WriteField("strip", "/m/")
	mw.WriteField("layer", "19")
	mw.Close()
	zw.Close()

	req := httptest.NewRequest(http.MethodPost, "/sae/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer push")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "{\"imported\":2}\n" {
		t.Fatalf("body: %q", rec.Body.String())
	}
	var model string
	var layer int
	if err := s.Store.DB().QueryRow(`SELECT model, layer FROM sae_encoders WHERE encoder='encT'`).Scan(&model, &layer); err != nil || model != "poolside/Laguna-XS-2.1-NVFP4" || layer != 19 {
		t.Fatalf("legacy import identity: %q %d %v", model, layer, err)
	}
	gz := filepath.Join(s.SaeDir, "daily-20260901", "sessions.html.gz")
	f, err := os.Open(gz)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if !bytes.Equal(plain, files["sessions.html"]) {
		t.Fatalf("page round trip: %d bytes", len(plain))
	}
	if _, err := os.Stat(filepath.Join(s.SaeDir, "daily-20260901", "sessions.html")); err == nil {
		t.Fatal("plain page should not be kept")
	}

	bad := httptest.NewRequest(http.MethodPost, "/sae/import", bytes.NewReader(nil))
	bad.Header.Set("Authorization", "Bearer push")
	bad.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, bad)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty import: %d", rr.Code)
	}
	_ = fmt.Sprint
}

func TestSaeImportInstallsTheWeightsItCarries(t *testing.T) {
	s, _, _ := newTestServer(t, "tok")
	s.PushToken = "push"
	s.SaeModelsDir = t.TempDir()
	h := s.Routes()
	weights := []byte("dictionary weights")
	sum := sha256.Sum256(weights)
	enc := hex.EncodeToString(sum[:])
	push := func(encoder string, scored []byte) int {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for name, content := range map[string][]byte{
			"result.json":        []byte(`{"encoder_sha256":"` + encoder + `","model":"vendor/m","layer":3,"config":{"width":4,"top_k":2}}`),
			"records.jsonl":      []byte(`{"id":"a","kind":"user","agent":"claude","host":"h","session":"s","time":"t","source":"/m/x.jsonl","line":1,"text":"hello"}` + "\n" + `{"id":"b","kind":"assistant","agent":"claude","host":"h","session":"s","time":"t","source":"/m/x.jsonl","line":2,"text":"world"}` + "\n"),
			"scored.safetensors": scored,
			"sae.safetensors":    weights,
		} {
			part, _ := mw.CreateFormFile(name, name)
			part.Write(content)
		}
		mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/sae/import", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer push")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	scored := tinySafetensors(t, 2, 2, 4)
	other := hex.EncodeToString(make([]byte, 32))
	if code := push(other, scored); code != http.StatusBadRequest {
		t.Fatalf("weights that do not hash to encoder_sha256: %d", code)
	}
	var n int
	s.Store.DB().QueryRow(`SELECT COUNT(*) FROM sae_encoders`).Scan(&n)
	if _, err := os.Stat(filepath.Join(s.SaeModelsDir, other)); err == nil || n != 0 {
		t.Fatalf("a refused import installed or imported something: %d encoders", n)
	}
	if code := push(enc, scored); code != http.StatusOK {
		t.Fatalf("import: %d", code)
	}
	dir := filepath.Join(s.SaeModelsDir, enc)
	if got, _ := os.ReadFile(filepath.Join(dir, "sae.safetensors")); !bytes.Equal(got, weights) {
		t.Fatal("weights not installed under the encoder id")
	}
	scale := func() []float32 {
		raw, err := os.ReadFile(filepath.Join(dir, "scale.safetensors"))
		if err != nil {
			t.Fatal(err)
		}
		hl := binary.LittleEndian.Uint64(raw[:8])
		var hdr map[string]struct {
			Shape   []int  `json:"shape"`
			Offsets [2]int `json:"data_offsets"`
		}
		if err := json.Unmarshal(raw[8:8+hl], &hdr); err != nil || len(hdr) != 1 || hdr["scale"].Shape[0] != 4 || (8+hl)%8 != 0 {
			t.Fatalf("scale header %v %v", hdr, err)
		}
		out := make([]float32, 4)
		binary.Read(bytes.NewReader(raw[8+hl:]), binary.LittleEndian, out)
		return out
	}
	if got := scale(); got[0] != 4 || got[3] != 4 {
		t.Fatalf("scale %v", got)
	}
	rescored := append([]byte(nil), scored...)
	for i := len(rescored) - 16; i < len(rescored); i += 4 {
		binary.LittleEndian.PutUint32(rescored[i:], math.Float32bits(9))
	}
	if code := push(enc, rescored); code != http.StatusOK {
		t.Fatalf("re-import: %d", code)
	}
	if got := scale(); got[0] != 4 {
		t.Fatalf("a later scoring run replaced the original scale: %v", got)
	}
	ix, err := sae.New(s.Store.DB())
	if err != nil {
		t.Fatal(err)
	}
	od, err := sae.NewOnDemand(ix, sae.ScorerConfig{CaptureURL: "http://capture.test", ModelsDir: s.SaeModelsDir})
	if err != nil || !od.Supports(enc) {
		t.Fatalf("on-demand scoring does not see the install: %v", err)
	}
}
