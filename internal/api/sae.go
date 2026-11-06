package api

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/eordano/cccp/internal/sae"
)

const saeImportMaxBytes = 4 << 30

var saeImportFiles = []string{"result.json", "scored.safetensors", "records.jsonl", "names.json"}

var saeLabelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s *Server) handleSaeImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, saeImportMaxBytes)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "gzip: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer zr.Close()
		r.Body = io.NopCloser(zr)
		r.Header.Del("Content-Encoding")
		r.ContentLength = -1
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	dir, err := os.MkdirTemp("", "cccp-sae-import-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)
	for _, name := range saeImportFiles {
		f, _, err := r.FormFile(name)
		if err != nil {
			if name == "names.json" {
				continue
			}
			http.Error(w, "missing file "+name, http.StatusBadRequest)
			return
		}
		out, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			f.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, err = io.Copy(out, f)
		f.Close()
		out.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	var layer int
	if v := r.FormValue("layer"); v != "" {
		var err error
		if layer, err = strconv.Atoi(v); err != nil || layer < 0 {
			http.Error(w, "bad layer", http.StatusBadRequest)
			return
		}
	}
	label := r.FormValue("label")
	if label != "" && !saeLabelRe.MatchString(label) {
		http.Error(w, "bad label", http.StatusBadRequest)
		return
	}
	if weights, _, err := r.FormFile("sae.safetensors"); err == nil && s.SaeModelsDir != "" {
		_, err := sae.InstallWeights(s.SaeModelsDir, dir, weights)
		weights.Close()
		if errors.Is(err, sae.ErrWeightsMismatch) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "sae.safetensors: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	n, err := sae.ImportWith(r.Context(), s.Store.DB(), dir, sae.ImportOptions{StripPrefix: r.FormValue("strip"), Model: r.FormValue("model"), Layer: layer, Label: label, Promote: r.FormValue("current") == "1" || label == ""})
	if err != nil {
		http.Error(w, "import: "+err.Error(), http.StatusBadRequest)
		return
	}
	if page, _, err := r.FormFile("sessions.html"); err == nil {
		defer page.Close()
		if err := s.saveSaePage(label, page); err != nil {
			http.Error(w, "sessions.html: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"imported":%d}`+"\n", n)
}

func (s *Server) handleSaeFeedbackExport(w http.ResponseWriter, r *http.Request) {
	ix, err := sae.New(s.Store.DB())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	if _, err := ix.ExportFeedback(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) saveSaePage(label string, page io.Reader) error {
	if s.SaeDir == "" {
		return fmt.Errorf("no --sae-dir on this server")
	}
	if label == "" {
		return fmt.Errorf("a sessions page needs a label")
	}
	dir := filepath.Join(s.SaeDir, label)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sessions-*.html.gz")
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(tmp)
	if _, err := io.Copy(zw, page); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "sessions.html.gz"))
}
