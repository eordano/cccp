package pravda

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eordano/cccp/internal/sae"
	"github.com/eordano/cccp/internal/store"
)

func (s *WatchServer) indexStore() *store.Store {
	if ss, ok := innerStore(s.Store).(*SQLiteStore); ok {
		return ss.store
	}
	return nil
}

func (s *WatchServer) registerIndexRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /sessions", s.handleSessionsJSON)
	mux.HandleFunc("GET /sessions/{id...}", s.handleSessionJSON)
	mux.HandleFunc("GET /index-panel/{path...}", s.handleIndexPanel)
	mux.HandleFunc("GET /files", s.handleFiles)
	mux.HandleFunc("GET /files/{path...}", s.handleFiles)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

type sessionJSON struct {
	store.SessionRow
	Started string        `json:"started,omitempty"`
	Ended   string        `json:"ended,omitempty"`
	Rollup  *store.Rollup `json:"rollup,omitempty"`
	URL     string        `json:"url"`
}

func toSessionJSON(r store.SessionRow, ru map[string]store.Rollup) sessionJSON {
	out := sessionJSON{SessionRow: r, URL: "/session/" + r.Path}
	if r.StartedAt > 0 {
		out.Started = time.Unix(0, r.StartedAt).UTC().Format(time.RFC3339)
	}
	if r.EndedAt > 0 {
		out.Ended = time.Unix(0, r.EndedAt).UTC().Format(time.RFC3339)
	}
	if v, ok := ru[r.Path]; ok && v.Descendants > 0 {
		out.Rollup = &v
	}
	return out
}

type indexStatus struct {
	Version    int    `json:"version"`
	Rebuilding bool   `json:"rebuilding"`
	RebuiltTo  string `json:"rebuild_path,omitempty"`
	Cursor     int64  `json:"cursor"`
	Updated    string `json:"updated,omitempty"`
}

func statusOf(m store.IndexMeta) indexStatus {
	st := indexStatus{Version: m.Version, Rebuilding: m.RebuildActive, Cursor: m.Cursor}
	if m.RebuildActive {
		st.RebuiltTo = m.RebuildPath
	}
	if m.UpdatedAt > 0 {
		st.Updated = time.Unix(0, m.UpdatedAt).UTC().Format(time.RFC3339)
	}
	return st
}

func (s *WatchServer) handleSessionsJSON(w http.ResponseWriter, r *http.Request) {
	st := s.indexStore()
	if st == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "the session index needs the SQLite store"})
		return
	}
	q := r.URL.Query()
	sq := store.SessionQuery{Agent: q.Get("agent"), Host: q.Get("host"), Project: q.Get("project"), Q: q.Get("q"),
		Parent: q.Get("parent"), All: q.Get("all") == "1"}
	sq.Limit, _ = strconv.Atoi(q.Get("limit"))
	if sq.Limit > 500 {
		sq.Limit = 500
	}
	sq.Offset, _ = strconv.Atoi(q.Get("offset"))
	if v := q.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			sq.Since = t.UnixNano()
		} else if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			sq.Since = n
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be RFC3339 or unix ns"})
			return
		}
	}
	ctx := r.Context()
	rows, total, err := st.ListSessionRows(ctx, sq)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	paths := make([]string, len(rows))
	for i, row := range rows {
		paths[i] = row.Path
	}
	ru, err := st.Rollups(ctx, paths)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := struct {
		Sessions []sessionJSON `json:"sessions"`
		Total    int           `json:"total"`
		Offset   int           `json:"offset"`
		Index    indexStatus   `json:"index"`
	}{Sessions: []sessionJSON{}, Total: total, Offset: sq.Offset}
	for _, row := range rows {
		out.Sessions = append(out.Sessions, toSessionJSON(row, ru))
	}
	if m, ok, err := st.IndexMeta(ctx); err == nil && ok {
		out.Index = statusOf(m)
	}
	writeJSON(w, http.StatusOK, out)
}

type recordMeta struct {
	ID         string `json:"id"`
	Ordinal    int    `json:"ordinal"`
	Kind       string `json:"kind"`
	Role       string `json:"role"`
	Subtype    string `json:"subtype,omitempty"`
	Tool       string `json:"tool,omitempty"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	TS         string `json:"ts,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	Model      string `json:"model,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	SubAgents  int    `json:"subagent_records,omitempty"`
	SaeID      string `json:"sae_id,omitempty"`
	SaeScored  bool   `json:"sae_scored,omitempty"`
	Preview    string `json:"preview,omitempty"`
}

func roleOf(kind string) string {
	switch kind {
	case KindUser, KindInterrupt, KindBashInput, KindCommand:
		return "user"
	case KindMessage, KindThinking:
		return "assistant"
	case KindTool, KindAgentResult, KindBashOutput, KindCommandOutput:
		return "tool"
	case KindMeta:
		return "meta"
	}
	return "system"
}

func durationOf(e *Entry) int64 {
	switch {
	case e.Kind == KindTool && e.AgentRef != nil && e.AgentRef.TotalDurationMs > 0:
		return e.AgentRef.TotalDurationMs
	case e.Kind == KindTool && e.RawResult != nil:
		a, b := parseNS(e.TS), parseNS(e.RawResult.Timestamp)
		if a > 0 && b >= a {
			return (b - a) / int64(time.Millisecond)
		}
	case e.DurationMs > 0:
		return e.DurationMs
	}
	return 0
}

func previewOf(e *Entry) string {
	t := ""
	switch e.Kind {
	case KindTool:
		t = toolPreview(e)
	case KindCommand:
		t = strings.TrimSpace(e.CommandName + " " + e.CommandArgs)
	default:
		t = firstLineEllipsis(e.Text, 120)
	}
	if !utf8.ValidString(t) {
		t = strings.ToValidUTF8(t, "")
	}
	return t
}

func recordsOf(sess *Session, sessionID string) []recordMeta {
	out := make([]recordMeta, 0, len(sess.Entries))
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.Index == 0 {
			continue
		}
		rec := recordMeta{
			ID: sessionID + "#" + strconv.Itoa(e.Index), Ordinal: e.Index, Kind: e.Kind, Role: roleOf(e.Kind),
			Subtype: e.Subtype, TS: e.TS, DurationMs: durationOf(e), IsError: e.IsError, Model: e.Model,
			SubAgents: len(e.SubAgent), Preview: previewOf(e),
		}
		if e.Kind == KindTool {
			rec.Tool, rec.ToolUseID = e.Name, e.ID
		}
		if e.RawSource != nil {
			rec.UUID = e.RawSource.UUID
			if rec.Model == "" && e.RawSource.Message != nil && rec.Role == "assistant" {
				rec.Model = e.RawSource.Message.Model
			}
		}
		if kind := saeKind(e); kind != "" {
			rec.SaeID = sae.RecordID(kind, saeText(e))
		}
		out = append(out, rec)
	}
	return out
}

func (s *WatchServer) loadSession(ctx context.Context, rel string) (*Session, error) {
	ver := s.pathVersion(rel)
	if sess, ok := s.parsed.getCachedSession(rel, ver); ok {
		return sess, nil
	}
	events, err := s.events(ctx, rel, ver)
	if err != nil {
		return nil, err
	}
	sess := Normalize(events, nil)
	setAgentPaths(sess, rel)
	sess.Facts = factsFromEvents(events)
	setEntryAgent(sess, agentOfPath(rel))
	s.parsed.setSession(rel, sess)
	return sess, nil
}

func (s *WatchServer) handleSessionJSON(w http.ResponseWriter, r *http.Request) {
	st := s.indexStore()
	if st == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "the session index needs the SQLite store"})
		return
	}
	ctx := r.Context()
	row, err := st.SessionRowByKey(ctx, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if row == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no indexed session with that path, id or session id"})
		return
	}
	if mode := r.URL.Query().Get("lines"); mode != "" {
		s.writeSessionLines(w, r, row.Path, mode)
		return
	}
	q := r.URL.Query()
	detail := &sessionDetail{Session: toSessionJSON(*row, nil), Children: []sessionJSON{}, Background: []bgFile{}}
	if q.Get("detail") != "0" {
		if detail, err = s.sessionDetail(ctx, st, row); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	if q.Get("records") == "1" {
		sess, err := s.loadSession(ctx, row.Path)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		recs := recordsOf(sess, row.ID)
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		detail.RecordsTotal = len(recs)
		if offset > 0 {
			recs = recs[min(offset, len(recs)):]
		}
		if limit > 0 && limit < len(recs) {
			recs = recs[:limit]
		}
		s.markSaeScored(ctx, recs)
		detail.Records = recs
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *WatchServer) writeSessionLines(w http.ResponseWriter, r *http.Request, rel, mode string) {
	src := s.Store
	switch mode {
	case "raw":
		src = innerStore(s.Store)
	case "converted":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lines must be raw or converted"})
		return
	}
	ls, ok := src.(sessionLineStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "session lines need the SQLite store"})
		return
	}
	lines, _, err := ls.SessionLines(r.Context(), rel, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, l := range lines {
		l = bytes.TrimRight(l, "\r\n")
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		if _, err := w.Write(l); err != nil {
			return
		}
		w.Write([]byte{'\n'})
	}
}

func (s *WatchServer) markSaeScored(ctx context.Context, recs []recordMeta) {
	if s.Sae == nil {
		return
	}
	enc, err := s.Sae.Current(ctx)
	if err != nil || enc == "" {
		return
	}
	var ids []string
	for _, r := range recs {
		if r.SaeID != "" {
			ids = append(ids, r.SaeID)
		}
	}
	for len(ids) > 0 {
		n := min(len(ids), 400)
		found, err := s.Sae.ByIDs(ctx, enc, ids[:n])
		ids = ids[n:]
		if err != nil {
			return
		}
		for i := range recs {
			if _, ok := found[recs[i].SaeID]; ok {
				recs[i].SaeScored = true
			}
		}
	}
}

type sessionDetail struct {
	Session      sessionJSON   `json:"session"`
	Parent       *sessionJSON  `json:"parent,omitempty"`
	Children     []sessionJSON `json:"children"`
	Rollup       store.Rollup  `json:"rollup"`
	Background   []bgFile      `json:"background"`
	Skills       []string      `json:"skills_listed,omitempty"`
	Invoked      []string      `json:"skills_invoked,omitempty"`
	Records      []recordMeta  `json:"records,omitempty"`
	RecordsTotal int           `json:"records_total,omitempty"`
}

func (s *WatchServer) sessionDetail(ctx context.Context, st *store.Store, row *store.SessionRow) (*sessionDetail, error) {
	ru, err := st.Rollups(ctx, []string{row.Path})
	if err != nil {
		return nil, err
	}
	d := &sessionDetail{Session: toSessionJSON(*row, nil), Rollup: ru[row.Path], Children: []sessionJSON{}}
	if row.ParentPath != "" {
		if p, err := st.SessionRowByKey(ctx, row.ParentPath); err == nil && p != nil {
			pj := toSessionJSON(*p, nil)
			d.Parent = &pj
		}
	}
	kids, _, err := st.ListSessionRows(ctx, store.SessionQuery{Parent: row.Path, Limit: 500})
	if err != nil {
		return nil, err
	}
	kidPaths := make([]string, len(kids))
	for i, k := range kids {
		kidPaths[i] = k.Path
	}
	kru, err := st.Rollups(ctx, kidPaths)
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		d.Children = append(d.Children, toSessionJSON(k, kru))
	}
	full, err := st.SessionStates(ctx, []string{row.Path})
	if err != nil {
		return nil, err
	}
	if fr := full[row.Path]; fr != nil {
		hosts, err := st.HostNames(ctx)
		if err != nil {
			return nil, err
		}
		if d.Background, err = sessionBackground(ctx, st, fr, hosts); err != nil {
			return nil, err
		}
		var f foldState
		_ = json.Unmarshal(fr.State, &f)
		d.Skills, d.Invoked = f.Skills, f.Invoked
	}
	if d.Background == nil {
		d.Background = []bgFile{}
	}
	return d, nil
}

type indexPanelData struct {
	Detail  *sessionDetail
	Missing bool
	Groups  []bgGroup
	Hidden  int
}

type bgGroup struct {
	Kind  string
	Files []bgFile
}

func (d *sessionDetail) StartedText() string { return fmtNS(d.Session.StartedAt) }
func (d *sessionDetail) EndedText() string   { return fmtNS(d.Session.EndedAt) }

func (s *WatchServer) handleIndexPanel(w http.ResponseWriter, r *http.Request) {
	st := s.indexStore()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if st == nil {
		return
	}
	ctx := r.Context()
	data := indexPanelData{}
	row, err := st.SessionRowByKey(ctx, r.PathValue("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if row == nil {
		data.Missing = true
	} else {
		if data.Detail, err = s.sessionDetail(ctx, st, row); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		idx := map[string]int{}
		for _, f := range data.Detail.Background {
			if f.Kind == bgSkill && !f.Invoked {
				data.Hidden++
			}
			i, ok := idx[f.Kind]
			if !ok {
				i = len(data.Groups)
				idx[f.Kind] = i
				data.Groups = append(data.Groups, bgGroup{Kind: f.Kind})
			}
			data.Groups[i].Files = append(data.Groups[i].Files, f)
		}
	}
	_ = tmpl.ExecuteTemplate(w, "index-panel", data)
}

type filesPage struct {
	History *fileHistory
	Groups  []filesGroup
	Error   string
}

type filesGroup struct {
	Kind  string
	Files []filesEntry
}

type filesEntry struct {
	Path     string
	Name     string
	Agent    string
	Versions int
	Latest   string
}

func wantsJSON(r *http.Request) bool {
	return r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json")
}

func (s *WatchServer) handleFiles(w http.ResponseWriter, r *http.Request) {
	st := s.indexStore()
	if st == nil {
		http.Error(w, "file history needs the SQLite store", http.StatusNotImplemented)
		return
	}
	ctx := r.Context()
	wire := r.PathValue("path")
	fp := &filesPage{}
	if wire == "" {
		groups, err := backgroundCatalog(ctx, st)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fp.Groups = groups
		if wantsJSON(r) {
			writeJSON(w, http.StatusOK, groups)
			return
		}
	} else {
		h, err := buildFileHistory(ctx, st, wire)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if h == nil {
			if wantsJSON(r) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "no synced versions of " + wire})
				return
			}
			http.NotFound(w, r)
			return
		}
		if wantsJSON(r) {
			writeJSON(w, http.StatusOK, h)
			return
		}
		if v, err := strconv.ParseInt(r.URL.Query().Get("v"), 10, 64); err == nil {
			for _, vv := range h.Versions {
				if vv.Version == v && vv.SHA != "" {
					if b, err := st.GetBlob(ctx, vv.SHA); err == nil {
						if len(b) > 256<<10 {
							b = b[:256<<10]
						}
						h.Content, h.ViewV = strings.ToValidUTF8(string(b), "�"), v
					}
				}
			}
		}
		fp.History = h
	}
	flat, err := s.buildSidebar(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flat, pager := paginateFlat(flat, 1, "/", "", "")
	page := pageData{Title: "pravda · background", FlatList: flat, Pager: pager, Files: fp, GroupBy: readGroup(r), HiddenKinds: readHidden(r)}
	page.Groups = groupItems(page.FlatList, page.GroupBy)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHTMX(r) {
		_ = tmpl.ExecuteTemplate(w, "main-content", page)
		return
	}
	_ = renderBase(w, page)
}

func backgroundCatalog(ctx context.Context, st *store.Store) ([]filesGroup, error) {
	idx := map[string]int{}
	var out []filesGroup
	seen := map[string]bool{}
	prefixes := []string{"CLAUDE.md", "memory/", "skills/", "agents/", "commands/", "settings.json", "hooks/"}
	agents, err := st.LogAgents(ctx)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, a := range append([]string{"codex", "hermes"}, agents...) {
		if !listed[a] {
			listed[a] = true
			prefixes = append(prefixes, agentBackgroundPrefixes(a)...)
		}
	}
	dirs, err := st.ClaudeProjectDirs(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		prefixes = append(prefixes, d+"memory/")
	}
	for _, prefix := range prefixes {
		paths, err := st.VersionedPaths(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			kind, agent, name := classifyBackground(p)
			if kind == "" {
				continue
			}
			latest, err := st.LatestVersion(ctx, p)
			if err != nil {
				return nil, err
			}
			if latest == nil || latest.ContentSHA == nil {
				continue
			}
			if agent == "claude" && kind == bgMemory && strings.HasPrefix(p, "projects/") {
				name = strings.TrimPrefix(claudeProjectDir(p), "projects/") + name
			}
			i, ok := idx[kind]
			if !ok {
				i = len(out)
				idx[kind] = i
				out = append(out, filesGroup{Kind: kind})
			}
			out[i].Files = append(out[i].Files, filesEntry{Path: p, Name: name, Agent: agent, Versions: int(latest.Version), Latest: fmtNS(latest.MtimeNS)})
		}
	}
	return out, nil
}
