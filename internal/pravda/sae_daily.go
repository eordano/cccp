package pravda

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *WatchServer) handleSaeDaily(w http.ResponseWriter, r *http.Request) {
	if s.SaeDir == "" {
		http.NotFound(w, r)
		return
	}
	rel := filepath.Clean("/" + r.PathValue("file"))
	if strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.SaeDir, rel)
	if strings.HasSuffix(rel, ".html") {
		if st, err := os.Stat(full + ".gz"); err == nil && !st.IsDir() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
			http.ServeFile(w, r, full+".gz")
			return
		}
	}
	http.ServeFile(w, r, full)
}
