package pravda

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *WatchServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	flat, err := s.buildSidebar(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query().Get("q")
	if q != "" {
		flat = filterFlatList(flat, q)
	}
	page := pageData{
		Title:       "pravda",
		FlatList:    flat,
		HiddenKinds: readHidden(r),
		JumpLines:   readBool(r, cookieJumpLines, true),
		JumpFile:    readBool(r, cookieJumpFile, true),
		SearchQuery: q,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHTMX(r) {
		_ = tmpl.ExecuteTemplate(w, "sidebar", page)
		return
	}
	_ = renderBase(w, page)
}

func filterFlatList(items []*flatItem, q string) []*flatItem {
	q = strings.ToLower(q)
	var out []*flatItem
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.File.Title), q) ||
			strings.Contains(strings.ToLower(item.File.Path), q) ||
			strings.Contains(strings.ToLower(item.Folder), q) {
			out = append(out, item)
		}
	}
	return out
}

func (s *WatchServer) handleSession(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	ctx := r.Context()

	ver := s.pathVersion(rel)
	sess, ok := s.parsed.getCachedSession(rel, ver)
	if !ok {
		events, cached := s.parsed.getCached(rel, ver)
		if !cached {
			rc, err := s.Store.OpenSession(ctx, rel)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer rc.Close()
			events, err = s.parsed.parse(rc, rel, ver)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		sess = Normalize(events, nil)
		setAgentPaths(sess, rel)
		s.parsed.setSession(rel, sess)
	}

	if subID := r.URL.Query().Get("subagent"); subID != "" {
		for _, e := range sess.Entries {
			if e.Kind == KindTool && e.ID == subID {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_ = tmpl.ExecuteTemplate(w, "subagent-entries", &Session{Entries: e.SubAgent})
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	s.cache.set(rel, fingerprintAll(sess.Entries))

	file := &fileInfo{
		Name: filepath.Base(rel),
		Path: rel,
	}
	file.IsAgent = isAgentPath(rel)
	if file.IsAgent {
		file.AgentID = strings.TrimSuffix(strings.TrimPrefix(file.Name, "agent-"), ".jsonl")
	}
	file.Title = extractTitleFromEvents(sess.Events)

	flat, err := s.buildSidebar(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hidden := readHidden(r)

	page := pageData{
		Title:       "pravda · " + file.Name,
		FlatList:    flat,
		CurrentPath: rel,
		Session:     sess,
		SessionFile: file,
		HiddenKinds: hidden,
		JumpLines:   readBool(r, cookieJumpLines, true),
		JumpFile:    readBool(r, cookieJumpFile, true),
	}

	if evStr := r.URL.Query().Get("event"); evStr != "" {
		idx, _ := strconv.Atoi(evStr)
		idx--
		if idx < 0 || idx >= len(sess.Entries) {
			http.NotFound(w, r)
			return
		}
		entry := &sess.Entries[idx]
		page.Title = "inspect · " + file.Name
		page.Session = nil
		page.Inspector = &inspectorData{
			Entry:   entry,
			Raw:     entry.RawSource,
			Result:  entry.RawResult,
			ToolUse: entry.RawToolUse,
			BackURL: "/session/" + rel,
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHTMX(r) {
		_ = tmpl.ExecuteTemplate(w, "main-content", page)
		return
	}
	_ = renderBase(w, page)
}

func isHTMX(r *http.Request) bool {
	if r.Header.Get("HX-Boosted") == "true" {
		return false
	}
	return r.Header.Get("HX-Request") == "true"
}

func safeBack(r *http.Request) string {
	ref := r.Referer()
	if ref == "" || strings.HasPrefix(ref, "//") {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/"
	}
	if u.IsAbs() {
		if u.Host != r.Host {
			return "/"
		}
		return pathWithQuery(u)
	}
	if !strings.HasPrefix(ref, "/") {
		return "/"
	}
	return ref
}

func pathWithQuery(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

func (s *WatchServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := r.PathValue("key")
	switch key {
	case "filter":
		kind := r.FormValue("kind")
		if kind == "" {
			http.Error(w, "missing kind", http.StatusBadRequest)
			return
		}
		list := readHidden(r)
		list = toggleInList(list, kind)
		writeStringList(w, cookieHidden, list)
	case "filter-reset":
		writeStringList(w, cookieHidden, defaultHidden)
	case "filter-clear":
		writeStringList(w, cookieHidden, []string{})
	case "collapse":
		folder := r.FormValue("folder")
		if folder == "" {
			http.Error(w, "missing folder", http.StatusBadRequest)
			return
		}
		list := readStringList(r, cookieCollapsed)
		list = toggleInList(list, folder)
		writeStringList(w, cookieCollapsed, list)
	case "jump-lines":
		writeBool(w, cookieJumpLines, r.FormValue("on") == "1")
	case "jump-file":
		writeBool(w, cookieJumpFile, r.FormValue("on") == "1")
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, safeBack(r), http.StatusSeeOther)
}

func (s *WatchServer) buildSidebar(ctx context.Context) ([]*flatItem, error) {

	s.sidebar.mu.RLock()
	if s.sidebar.valid {
		fl := s.sidebar.flat
		s.sidebar.mu.RUnlock()
		return fl, nil
	}
	s.sidebar.mu.RUnlock()

	groups, err := s.collectSessions(ctx)
	if err != nil {
		return nil, err
	}
	var flat []*flatItem
	for _, g := range groups {
		folder := prettyFolder(filepath.ToSlash(filepath.Dir(g.Session.Path)))
		item := &flatItem{File: g.Session, Folder: folder}
		for _, a := range g.Agents {
			item.Agents = append(item.Agents, a)
		}
		flat = append(flat, item)
	}
	sort.Slice(flat, func(i, j int) bool { return flat[i].File.Modified > flat[j].File.Modified })

	s.sidebar.mu.Lock()
	s.sidebar.flat = flat
	s.sidebar.valid = true
	s.sidebar.mu.Unlock()

	return flat, nil
}

func (s *WatchServer) collectSessions(ctx context.Context) ([]*sessionGroup, error) {
	metas, err := s.Store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}

	var needTitle []int
	for i := range metas {
		if cached, ok := s.titles.Load(metas[i].Path); ok {
			if title, ok := cached.(string); ok {
				metas[i].Title = title
				continue
			}
		}
		if !isAgentPath(metas[i].Path) {
			needTitle = append(needTitle, i)
		}
	}
	if len(needTitle) > 0 {
		s.extractMissingTitles(ctx, metas, needTitle)
	}
	sessions := make(map[string]*sessionGroup)
	for _, m := range metas {
		if filepath.Base(m.Path) == "history.jsonl" {
			continue
		}
		fi := metaToFileInfo(m)
		if isAgentPath(m.Path) {
			parentPath := parentSessionOf(m.Path)
			if sessions[parentPath] == nil {
				sessions[parentPath] = &sessionGroup{}
			}
			sessions[parentPath].Agents = append(sessions[parentPath].Agents, fi)
		} else {
			if sessions[m.Path] == nil {
				sessions[m.Path] = &sessionGroup{}
			}
			sessions[m.Path].Session = fi
		}
	}
	out := make([]*sessionGroup, 0, len(sessions))
	for path, g := range sessions {
		if g.Session == nil {
			g.Session = &fileInfo{Name: filepath.Base(path), Path: path}
		}
		sort.Slice(g.Agents, func(i, j int) bool { return g.Agents[i].Modified < g.Agents[j].Modified })
		out = append(out, g)
	}
	return out, nil
}

func metaToFileInfo(m SessionMeta) *fileInfo {
	name := filepath.Base(m.Path)
	fi := &fileInfo{
		ID:       strings.NewReplacer("/", "-", ".", "-").Replace(m.Path),
		Name:     name,
		Path:     m.Path,
		Size:     m.Size,
		Modified: m.ModTime.Format(time.RFC3339),
		Title:    m.Title,
		IsAgent:  isAgentPath(m.Path),
	}
	if fi.IsAgent {
		fi.AgentID = strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
	}
	return fi
}

func setAgentPaths(sess *Session, parentRel string) {
	base := strings.TrimSuffix(parentRel, filepath.Ext(parentRel))
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.Kind != KindTool || (e.Name != "Agent" && e.Name != "Task") {
			continue
		}
		if len(e.SubAgent) > 0 {
			e.ParentPath = parentRel
		} else if e.AgentRef != nil && e.AgentRef.AgentID != "" {
			e.SubAgentPath = filepath.ToSlash(filepath.Join(base, "subagents", "agent-"+e.AgentRef.AgentID+".jsonl"))
		}
	}
}

func (s *WatchServer) handleSubAgent(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	ctx := r.Context()
	ver := s.pathVersion(rel)
	events, cached := s.parsed.getCached(rel, ver)
	if !cached {
		rc, err := s.Store.OpenSession(ctx, rel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer rc.Close()
		events, err = s.parsed.parse(rc, rel, ver)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	sess := Normalize(events, nil)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "subagent-entries", sess)
}

var agentPathRe = regexp.MustCompile(`^(.+?)/([^/]+?)/subagents/agent-(.+)\.jsonl$`)

func isAgentPath(rel string) bool { return agentPathRe.MatchString(rel) }

func parentSessionOf(rel string) string {
	rel = filepath.ToSlash(rel)
	m := agentPathRe.FindStringSubmatch(rel)
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2] + ".jsonl"
}

type sessionHeadOpener interface {
	OpenSessionHead(ctx context.Context, path string, limit int) (io.ReadCloser, error)
}

func (s *WatchServer) extractMissingTitles(ctx context.Context, metas []SessionMeta, indices []int) {
	type result struct {
		idx   int
		title string
	}
	ch := make(chan result, len(indices))
	sem := make(chan struct{}, 16)
	for _, idx := range indices {
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem }()
			t := s.extractSessionTitle(ctx, metas[i].Path)
			ch <- result{i, t}
		}(idx)
	}
	for range indices {
		r := <-ch
		metas[r.idx].Title = r.title
		if r.title != "" {
			s.titles.Store(metas[r.idx].Path, r.title)
		}
	}
}

func (s *WatchServer) extractSessionTitle(ctx context.Context, path string) string {

	if ho, ok := s.Store.(sessionHeadOpener); ok {
		rc, err := ho.OpenSessionHead(ctx, path, 20)
		if err == nil {
			defer rc.Close()
			return extractTitleFromReader(rc)
		}
	}
	rc, err := s.Store.OpenSession(ctx, path)
	if err != nil {
		return ""
	}
	defer rc.Close()
	return extractTitleFromReader(rc)
}

func extractTitleFromEvents(events []*RawEvent) string {
	for _, e := range events {
		if t := userPromptTitle(e); t != "" {
			return t
		}
	}
	var model string
	for _, e := range events {
		if t := assistantFirstLineTitle(e); t != "" {
			return t
		}
		if model == "" && e.Type == "assistant" && e.Message != nil && e.Message.Model != "" {
			model = e.Message.Model
		}
	}
	return model
}

func extractTitleFromReader(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	var assistantFallback string
	var modelFallback string
	lines := 0
	for sc.Scan() && lines < 500 {
		lines++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e RawEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if t := userPromptTitle(&e); t != "" {
			return t
		}
		if assistantFallback == "" {
			assistantFallback = assistantFirstLineTitle(&e)
			if modelFallback == "" && e.Type == "assistant" && e.Message != nil && e.Message.Model != "" {
				modelFallback = e.Message.Model
			}
		}
	}
	if assistantFallback != "" {
		return assistantFallback
	}
	return modelFallback
}

func userPromptTitle(e *RawEvent) string {
	if e == nil || e.Type != "user" || e.IsMeta || e.Message == nil {
		return ""
	}
	s := decodeString(e.Message.Content)
	if s == "" || strings.HasPrefix(s, "<") {
		return ""
	}
	return firstLineEllipsis(s, 90)
}

func assistantFirstLineTitle(e *RawEvent) string {
	if e == nil || e.Type != "assistant" || e.Message == nil {
		return ""
	}
	for _, item := range decodeContent(e.Message.Content) {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			return firstLineEllipsis(item.Text, 90)
		}
	}
	return ""
}

func firstLineEllipsis(s string, max int) string {
	line := strings.TrimSpace(firstLine(s))
	if line == "" {
		return ""
	}
	if len(line) > max {
		return line[:max] + "..."
	}
	return line
}

func prettyFolder(dir string) string {
	if dir == "" {
		return "Root"
	}
	last := filepath.Base(dir)
	if strings.HasPrefix(last, "-Users-") {
		rest := strings.SplitN(last, "-", 4)
		if len(rest) == 4 {
			return strings.ReplaceAll(rest[3], "-", "/")
		}
	}
	return last
}
