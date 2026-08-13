package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/store"
)

type ManifestEntry struct {
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	MtimeNS int64  `json:"mtime_ns"`
}

type ingestResponse struct {
	Need      []string `json:"need"`
	Committed int      `json:"committed"`
	Skipped   int      `json:"skipped"`
	Pending   int      `json:"pending"`
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	hostID := r.Header.Get(HeaderHostID)
	if hostID == "" {
		httpError(w, http.StatusBadRequest, "missing "+HeaderHostID)
		return
	}
	hostName := r.Header.Get(HeaderHostName)
	ctx := r.Context()
	if err := s.Store.UpsertHost(ctx, hostID, hostName); err != nil {
		httpError(w, http.StatusInternalServerError, "host upsert: "+err.Error())
		return
	}

	resp := ingestResponse{Need: []string{}}
	now := time.Now().UnixNano()
	seenNeed := map[string]struct{}{}

	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, scanBufInit), maxScanLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e ManifestEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			httpError(w, http.StatusBadRequest, "bad manifest line: "+err.Error())
			return
		}
		if err := validatePath(e.Path); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}

		isDelete := e.SHA == ""
		if !isDelete {
			ok, err := s.Store.HasBlob(ctx, e.SHA)
			if err != nil {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !ok {
				if _, dup := seenNeed[e.SHA]; !dup {
					seenNeed[e.SHA] = struct{}{}
					resp.Need = append(resp.Need, e.SHA)
				}
				resp.Pending++
				continue
			}
		}

		latest, err := s.Store.LatestVersion(ctx, e.Path)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if latest != nil && sameContent(latest, e, isDelete) {
			resp.Skipped++
			continue
		}
		if latest != nil && latest.ContentSHA == nil && !isDelete && e.MtimeNS <= latest.IngestedAt {
			resp.Skipped++
			continue
		}

		var nextVersion int64 = 1
		if latest != nil {
			nextVersion = latest.Version + 1
		}
		fv := store.FileVersion{
			Path:       e.Path,
			Version:    nextVersion,
			Mode:       e.Mode,
			MtimeNS:    e.MtimeNS,
			HostID:     hostID,
			IngestedAt: now,
		}
		if !isDelete {
			sha := e.SHA
			fv.ContentSHA = &sha
		}
		if err := s.Store.AppendVersion(ctx, fv); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp.Committed++
	}
	if resp.Committed > 0 {
		s.Events.Publish(Event{Kind: "blob", Host: hostID})
	}
	if err := scanner.Err(); err != nil {
		httpError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	s.Log.Info("ingest",
		"host", hostID,
		"committed", resp.Committed,
		"skipped", resp.Skipped,
		"pending", resp.Pending,
		"need", len(resp.Need))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func sameContent(latest *store.FileVersion, e ManifestEntry, isDelete bool) bool {
	if isDelete {
		return latest.ContentSHA == nil
	}
	return latest.ContentSHA != nil && *latest.ContentSHA == e.SHA
}

func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("path contains NUL")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("absolute path not allowed: %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path escapes root: %q", p)
		}
	}
	return nil
}
