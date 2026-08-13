package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/eordano/cccp/internal/store"
)

type LogAppendEntry struct {
	Path          string `json:"path"`
	Line          string `json:"line"`
	SuggestedTime int64  `json:"suggested_time"`
}

type logAppendResponse struct {
	New int `json:"new"`
	Dup int `json:"dup"`
}

func (s *Server) handleLogAppend(w http.ResponseWriter, r *http.Request) {
	hostID := r.Header.Get(HeaderHostID)
	if hostID == "" {
		httpError(w, http.StatusBadRequest, "missing "+HeaderHostID)
		return
	}
	ctx := r.Context()
	if err := s.Store.UpsertHost(ctx, hostID, r.Header.Get(HeaderHostName)); err != nil {
		httpError(w, http.StatusInternalServerError, "host upsert: "+err.Error())
		return
	}

	var resp logAppendResponse
	changedPaths := map[string]struct{}{}
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, scanBufInit), maxScanLine)
	for scanner.Scan() {
		raw := strings.TrimRight(scanner.Text(), "\r")
		if raw == "" {
			continue
		}
		var e LogAppendEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			httpError(w, http.StatusBadRequest, "bad log line: "+err.Error())
			return
		}
		if err := validatePath(e.Path); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		sum := sha256.Sum256([]byte(e.Line))
		l := store.LogLine{
			Path:          e.Path,
			LineHash:      hex.EncodeToString(sum[:]),
			Contents:      []byte(e.Line),
			SuggestedTime: e.SuggestedTime,
			FirstHost:     hostID,
		}
		ins, err := s.Store.AppendLogLine(ctx, l)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ins {
			resp.New++
			changedPaths[e.Path] = struct{}{}
		} else {
			resp.Dup++
		}
	}
	if err := scanner.Err(); err != nil {
		httpError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if resp.New > 0 {
		paths := make([]string, 0, len(changedPaths))
		for p := range changedPaths {
			paths = append(paths, p)
		}
		s.Events.Publish(Event{Kind: "log", Host: hostID, Paths: paths})
	}

	if resp.New > 0 {
		s.Log.Info("log append",
			"host", hostID,
			"new", resp.New,
			"dup", resp.Dup,
			"paths", len(changedPaths))
	} else {
		s.Log.Debug("log append",
			"host", hostID,
			"new", resp.New,
			"dup", resp.Dup,
			"paths", len(changedPaths))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type logSinceLine struct {
	Path          string `json:"path"`
	Line          string `json:"line"`
	SuggestedTime int64  `json:"suggested_time"`
	FirstSeen     int64  `json:"first_seen"`
}

func (s *Server) handleLogSince(w http.ResponseWriter, r *http.Request) {
	sinceStr := r.URL.Query().Get("since")
	var since int64
	if sinceStr != "" {
		v, err := strconv.ParseInt(sinceStr, 10, 64)
		if err != nil {
			httpError(w, http.StatusBadRequest, "since must be integer ns")
			return
		}
		since = v
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	var sent int
	err := s.Store.LogLinesSince(r.Context(), since, func(l store.LogLine) bool {
		if enc.Encode(logSinceLine{
			Path:          l.Path,
			Line:          string(l.Contents),
			SuggestedTime: l.SuggestedTime,
			FirstSeen:     l.FirstSeen,
		}) != nil {
			return false
		}
		sent++
		return true
	})
	if err != nil {
		s.Log.Error("log since", "err", err)
		return
	}
	s.Log.Info("log since",
		"host", r.Header.Get(HeaderHostID),
		"since", since,
		"lines", sent)
}
