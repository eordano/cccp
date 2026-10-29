package api

import (
	"archive/tar"
	"bytes"
	"net/http"
	"strconv"
	"time"

	"github.com/eordano/cccp/internal/syncclass"
)

func (s *Server) handleCurrent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	gated := r.Header.Get(syncclass.GateHeader) == "1"
	var entries []snapEntry
	var err error
	if gated {
		entries, err = s.gatedSnapshot(ctx)
	} else {
		entries, err = s.legacySnapshot(ctx)
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if e.Deleted {
			if err := tw.WriteHeader(&tar.Header{
				Name:       e.Path,
				Mode:       int64(e.Mode & 0o7777),
				ModTime:    time.Unix(0, e.MtimeNS),
				Typeflag:   tar.TypeReg,
				Format:     tar.FormatPAX,
				PAXRecords: map[string]string{syncclass.PAXDeleted: "1"},
			}); err != nil {
				httpError(w, http.StatusInternalServerError, "tar header: "+err.Error())
				return
			}
			continue
		}
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

	if gated {
		w.Header().Set(syncclass.GateHeader, "1")
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="current.tar"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Write(buf.Bytes())
	s.Log.Info("current snapshot",
		"host", r.Header.Get(HeaderHostID),
		"files", len(entries),
		"gated", gated,
		"bytes", buf.Len())
}
