package pravda

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSaeDailyServesGzipPage(t *testing.T) {
	dir := t.TempDir()
	page := bytes.Repeat([]byte("<p>stance</p>"), 1000)
	os.MkdirAll(filepath.Join(dir, "daily-20260901"), 0o755)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(page)
	zw.Close()
	os.WriteFile(filepath.Join(dir, "daily-20260901", "sessions.html.gz"), gz.Bytes(), 0o644)
	os.WriteFile(filepath.Join(dir, "plain.html"), page, 0o644)

	ws := &WatchServer{SaeDir: dir}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sae/daily/{file...}", ws.handleSaeDaily)
	for _, tc := range []struct {
		path string
		code int
		gzip bool
	}{
		{"/sae/daily/daily-20260901/sessions.html", 200, true},
		{"/sae/daily/plain.html", 200, false},
		{"/sae/daily/daily-20260901/missing.html", 404, false},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != tc.code {
			t.Fatalf("%s: %d want %d", tc.path, rr.Code, tc.code)
		}
		if tc.code != 200 {
			continue
		}
		var body []byte
		if tc.gzip {
			if rr.Header().Get("Content-Encoding") != "gzip" || rr.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("%s headers: %v", tc.path, rr.Header())
			}
			zr, err := gzip.NewReader(rr.Body)
			if err != nil {
				t.Fatal(err)
			}
			body, _ = io.ReadAll(zr)
		} else {
			if rr.Header().Get("Content-Encoding") != "" {
				t.Fatalf("%s should be plain: %v", tc.path, rr.Header())
			}
			body = rr.Body.Bytes()
		}
		if !bytes.Equal(body, page) {
			t.Fatalf("%s: served %d bytes", tc.path, len(body))
		}
	}
}
