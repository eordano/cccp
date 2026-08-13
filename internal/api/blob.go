package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
)

func (s *Server) handlePutBlob(w http.ResponseWriter, r *http.Request) {
	want := strings.ToLower(r.PathValue("sha"))
	if !isHexSHA256(want) {
		httpError(w, http.StatusBadRequest, "sha must be 64 hex chars (sha256)")
		return
	}
	ctx := r.Context()

	if ok, err := s.Store.HasBlob(ctx, want); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	} else if ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	limited := http.MaxBytesReader(w, r.Body, MaxBlobBytes)
	defer limited.Close()

	h := sha256.New()
	body, err := io.ReadAll(io.TeeReader(limited, h))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpError(w, http.StatusRequestEntityTooLarge, "blob exceeds 1 GiB cap")
			return
		}
		httpError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		httpError(w, http.StatusBadRequest, "sha mismatch: body hashes to "+got)
		return
	}

	if int64(len(body)) > WarnBlobBytes {
		s.Log.Warn("large blob uploaded", "sha", want, "bytes", len(body))
	}

	if err := s.Store.PutBlob(ctx, want, body); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("blob stored",
		"host", r.Header.Get(HeaderHostID),
		"sha", want[:12],
		"bytes", len(body))
	w.WriteHeader(http.StatusCreated)
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
