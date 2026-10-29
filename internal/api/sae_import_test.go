package api

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
