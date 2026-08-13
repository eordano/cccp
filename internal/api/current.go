package api

import (
	"archive/tar"
	"bytes"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) handleCurrent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	entries, err := s.Store.CurrentSnapshot(ctx)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		body, err := s.Store.GetBlob(ctx, e.ContentSHA)
		if err != nil {
			s.Log.Error("snapshot blob missing", "path", e.Path, "sha", e.ContentSHA, "err", err)
			httpError(w, http.StatusInternalServerError, "snapshot blob: "+err.Error())
			return
		}
		hdr := &tar.Header{
			Name:     e.Path,
			Mode:     int64(e.Mode & 0o7777),
			Size:     int64(len(body)),
			ModTime:  time.Unix(0, e.MtimeNS),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			httpError(w, http.StatusInternalServerError, "tar header: "+err.Error())
			return
		}
		if _, err := tw.Write(body); err != nil {
			httpError(w, http.StatusInternalServerError, "tar body: "+err.Error())
			return
		}
	}
	if err := tw.Close(); err != nil {
		httpError(w, http.StatusInternalServerError, "tar close: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="current.tar"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Write(buf.Bytes())
	s.Log.Info("current snapshot",
		"host", r.Header.Get(HeaderHostID),
		"files", len(entries),
		"bytes", buf.Len())
}
