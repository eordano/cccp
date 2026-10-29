package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/store"
	"github.com/eordano/cccp/internal/syncclass"
)

const HeaderSessions = "X-Cccp-Sessions"

type snapEntry struct {
	store.CurrentEntry
	Deleted bool
}

func (s *Server) gatedSnapshot(ctx context.Context) ([]snapEntry, error) {
	latest, err := s.Store.CurrentSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var out []snapEntry
	for _, e := range latest {
		if syncclass.Classify(e.Path, e.Mode) == syncclass.ClassTranscript {
			out = append(out, snapEntry{CurrentEntry: e})
		}
	}
	reviews, err := s.Store.Reviews(ctx, "")
	if err != nil {
		return nil, err
	}
	for path, rs := range reviews {
		v, ok := store.EffectiveVersion(rs)
		if !ok {
			continue
		}
		fv, err := s.Store.Version(ctx, path, v)
		if err != nil {
			return nil, err
		}
		if syncclass.Classify(path, fv.Mode) != syncclass.ClassKnowledge {
			continue
		}
		e := snapEntry{CurrentEntry: store.CurrentEntry{Path: path, Mode: fv.Mode, MtimeNS: fv.MtimeNS}}
		if fv.ContentSHA == nil {
			e.Deleted = true
		} else {
			e.ContentSHA = *fv.ContentSHA
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *Server) legacySnapshot(ctx context.Context) ([]snapEntry, error) {
	latest, err := s.Store.CurrentSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	gate, err := s.Store.GateSince(ctx)
	if err != nil {
		return nil, err
	}
	frozen, err := s.Store.LatestAsOf(ctx, gate)
	if err != nil {
		return nil, err
	}
	var out []snapEntry
	for _, e := range latest {
		if syncclass.Classify(e.Path, e.Mode) == syncclass.ClassTranscript {
			out = append(out, snapEntry{CurrentEntry: e})
		}
	}
	for _, e := range frozen {
		if syncclass.Classify(e.Path, e.Mode) != syncclass.ClassTranscript {
			out = append(out, snapEntry{CurrentEntry: e})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type ReviewItem struct {
	Path       string          `json:"path"`
	Version    int64           `json:"version"`
	Class      string          `json:"class"`
	Status     string          `json:"status"`
	SHA        string          `json:"sha,omitempty"`
	Deleted    bool            `json:"deleted,omitempty"`
	Mode       uint32          `json:"mode"`
	HostID     string          `json:"host_id"`
	Host       string          `json:"host,omitempty"`
	Agent      string          `json:"agent,omitempty"`
	Sessions   string          `json:"sessions,omitempty"`
	IngestedAt int64           `json:"ingested_at"`
	Superseded bool            `json:"superseded,omitempty"`
	Effective  int64           `json:"effective,omitempty"`
	Precheck   *store.Precheck `json:"precheck,omitempty"`
}

const (
	statusPending  = "pending"
	statusArchived = "archived"
	statusLegacy   = "legacy"
)

func (s *Server) reviewItem(ctx context.Context, fv store.FileVersion, rs []store.Review, gate int64, hosts map[string]string) ReviewItem {
	it := ReviewItem{
		Path: fv.Path, Version: fv.Version, Mode: fv.Mode,
		HostID: fv.HostID, Host: hosts[fv.HostID], IngestedAt: fv.IngestedAt,
	}
	if fv.ContentSHA != nil {
		it.SHA = *fv.ContentSHA
	} else {
		it.Deleted = true
	}
	class := syncclass.Classify(fv.Path, fv.Mode)
	it.Class = class.String()
	if eff, ok := store.EffectiveVersion(rs); ok {
		it.Effective = eff
	}
	switch {
	case class == syncclass.ClassPrivileged, class == syncclass.ClassKnowledge && syncclass.OwnSpace(fv.Path):
		it.Status = statusArchived
	case store.Decision(rs, fv.Version) != "":
		it.Status = store.Decision(rs, fv.Version)
	case fv.IngestedAt <= gate:
		it.Status = statusLegacy
	default:
		it.Status = statusPending
	}
	if m, err := s.Store.VersionMeta(ctx, fv.Path, fv.Version); err == nil {
		it.Agent, it.Sessions = m.Agent, m.Sessions
	}
	if p, err := s.Store.Precheck(ctx, fv.Path, fv.Version); err == nil {
		it.Precheck = p
	}
	return it
}

func (s *Server) reviewQueue(ctx context.Context, all bool) ([]ReviewItem, error) {
	gate, err := s.Store.GateSince(ctx)
	if err != nil {
		return nil, err
	}
	versions, err := s.Store.VersionsSince(ctx, gate)
	if err != nil {
		return nil, err
	}
	reviews, err := s.Store.Reviews(ctx, "")
	if err != nil {
		return nil, err
	}
	hosts, err := s.Store.ReviewHosts(ctx)
	if err != nil {
		return nil, err
	}
	newest := map[string]int64{}
	for _, fv := range versions {
		newest[fv.Path] = max(newest[fv.Path], fv.Version)
	}
	effSHA := map[string]string{}
	out := []ReviewItem{}
	for _, fv := range versions {
		if syncclass.Classify(fv.Path, fv.Mode) == syncclass.ClassTranscript {
			continue
		}
		it := s.reviewItem(ctx, fv, reviews[fv.Path], gate, hosts)
		it.Superseded = fv.Version < newest[fv.Path]
		if !all {
			if it.Status != statusPending {
				continue
			}
			if it.Effective != 0 {
				sha, ok := effSHA[fv.Path]
				if !ok {
					if ev, err := s.Store.Version(ctx, fv.Path, it.Effective); err == nil && ev.ContentSHA != nil {
						sha = *ev.ContentSHA
					}
					effSHA[fv.Path] = sha
				}
				if sha != "" && sha == it.SHA {
					continue
				}
			}
		}
		out = append(out, it)
	}
	return out, nil
}

type ReviewDetail struct {
	Item      ReviewItem     `json:"item"`
	Content   string         `json:"content"`
	Binary    bool           `json:"binary,omitempty"`
	Diff      string         `json:"diff"`
	DiffBase  int64          `json:"diff_base"`
	History   []ReviewItem   `json:"history"`
	Decisions []store.Review `json:"decisions"`
}

func (s *Server) blobText(ctx context.Context, sha *string) (string, error) {
	if sha == nil {
		return "", nil
	}
	b, err := s.Store.GetBlob(ctx, *sha)
	return string(b), err
}

func (s *Server) reviewDetail(ctx context.Context, path string, version int64) (*ReviewDetail, error) {
	fv, err := s.Store.Version(ctx, path, version)
	if err != nil {
		return nil, err
	}
	gate, err := s.Store.GateSince(ctx)
	if err != nil {
		return nil, err
	}
	reviews, err := s.Store.Reviews(ctx, path)
	if err != nil {
		return nil, err
	}
	hosts, err := s.Store.ReviewHosts(ctx)
	if err != nil {
		return nil, err
	}
	rs := reviews[path]
	d := &ReviewDetail{Item: s.reviewItem(ctx, *fv, rs, gate, hosts), Decisions: rs}
	content, err := s.blobText(ctx, fv.ContentSHA)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte([]byte(content), 0) >= 0 {
		d.Binary = true
		content = ""
	}
	d.Content = content

	var base string
	if d.Item.Effective != 0 && d.Item.Effective != version {
		d.DiffBase = d.Item.Effective
	} else if version > 1 {
		d.DiffBase = version - 1
	}
	if d.DiffBase != 0 {
		if bv, err := s.Store.Version(ctx, path, d.DiffBase); err == nil {
			if base, err = s.blobText(ctx, bv.ContentSHA); err != nil {
				return nil, err
			}
		}
	}
	if !d.Binary {
		d.Diff = unifiedDiff(path+"@v"+strconv.FormatInt(d.DiffBase, 10), path+"@v"+strconv.FormatInt(version, 10), base, content)
	}

	all, err := s.Store.Versions(ctx, path)
	if err != nil {
		return nil, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		d.History = append(d.History, s.reviewItem(ctx, all[i], rs, gate, hosts))
	}
	return d, nil
}

type decideRequest struct {
	Path     string `json:"path"`
	Version  int64  `json:"version"`
	Decision string `json:"decision"`
	Reviewer string `json:"reviewer"`
	Note     string `json:"note"`
}

var errNotReviewable = errors.New("only knowledge files are reviewable; privileged paths come from the repo and are never applied from cccp")

var errOwnSpace = errors.New("knowledge in an agent's own space is archived, not reviewed: only the home that writes the space pulls it")

var errReadOnly = errors.New("forbidden: read-only token cannot decide reviews")

func (s *Server) decide(ctx context.Context, req decideRequest) error {
	if readOnly(ctx) {
		return errReadOnly
	}
	fv, err := s.Store.Version(ctx, req.Path, req.Version)
	if err != nil {
		return err
	}
	if syncclass.Classify(fv.Path, fv.Mode) != syncclass.ClassKnowledge {
		return errNotReviewable
	}
	if syncclass.OwnSpace(fv.Path) {
		return errOwnSpace
	}
	if strings.TrimSpace(req.Reviewer) == "" {
		req.Reviewer = "unknown"
	}
	if err := s.Store.AddReview(ctx, store.Review{
		Path: req.Path, Version: req.Version, Decision: req.Decision,
		Reviewer: req.Reviewer, Note: req.Note,
	}); err != nil {
		return err
	}
	s.Log.Info("review", "path", req.Path, "version", req.Version, "decision", req.Decision, "reviewer", req.Reviewer)
	s.Events.Publish(Event{Kind: "blob", Host: "review"})
	return nil
}

func (s *Server) runPrecheck(path string, version int64, sha string, hostID, agentName string) {
	if s.Precheck == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	body, err := s.Store.GetBlob(ctx, sha)
	if err != nil {
		s.Log.Warn("precheck: blob", "path", path, "err", err)
		return
	}
	cmd := exec.CommandContext(ctx, s.Precheck)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(cmd.Environ(),
		"CCCP_PATH="+path,
		"CCCP_VERSION="+strconv.FormatInt(version, 10),
		"CCCP_SHA="+sha,
		"CCCP_HOST_ID="+hostID,
		"CCCP_AGENT="+agentName,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	verdict := "pass"
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		verdict = "flag"
	default:
		verdict = "error"
	}
	detail := out.String()
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	if runErr != nil && detail == "" {
		detail = runErr.Error()
	}
	if err := s.Store.PutPrecheck(context.Background(), path, version, store.Precheck{Verdict: verdict, Detail: detail}); err != nil {
		s.Log.Warn("precheck: record", "path", path, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func reviewStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNoVersion):
		return http.StatusNotFound
	case errors.Is(err, errNotReviewable), errors.Is(err, errOwnSpace), errors.Is(err, errReadOnly):
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}

func (s *Server) handleReviewList(w http.ResponseWriter, r *http.Request) {
	items, err := s.reviewQueue(r.Context(), r.URL.Query().Get("all") == "1")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, items)
}

func parsePathVersion(r *http.Request) (string, int64, error) {
	path := r.URL.Query().Get("path")
	v, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
	if path == "" || err != nil {
		return "", 0, errors.New("path and integer version are required")
	}
	return path, v, nil
}

func (s *Server) handleReviewShow(w http.ResponseWriter, r *http.Request) {
	path, v, err := parsePathVersion(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	d, err := s.reviewDetail(r.Context(), path, v)
	if err != nil {
		httpError(w, reviewStatus(err), err.Error())
		return
	}
	writeJSON(w, d)
}

func (s *Server) handleReviewDecide(w http.ResponseWriter, r *http.Request) {
	var req decideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if err := s.decide(r.Context(), req); err != nil {
		httpError(w, reviewStatus(err), err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
