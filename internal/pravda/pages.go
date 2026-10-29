package pravda

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const sessionsPerPage = 50

func (s *WatchServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	flat, err := s.buildSidebar(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query().Get("q")
	agent := r.URL.Query().Get("agent")
	counts := countAgents(flat)
	if agent != "" {
		flat = filterByAgent(flat, agent)
	}
	if q != "" {
		flat = filterFlatList(flat, q)
	}
	pageNum, _ := strconv.Atoi(r.URL.Query().Get("p"))
	flat, pager := paginateFlat(flat, pageNum, "/", q, agent)
	page := pageData{
		Title:       "pravda",
		FlatList:    flat,
		Pager:       pager,
		HiddenKinds: readHidden(r),
		JumpLines:   readBool(r, cookieJumpLines, true),
		JumpFile:    readBool(r, cookieJumpFile, true),
		SearchQuery: q,
		AgentFilter: agent,
		AgentCounts: counts,
		GroupBy:     readGroup(r),
	}
	page.Groups = groupItems(page.FlatList, page.GroupBy)
	page.Preset = presetOf(page.HiddenKinds)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHTMX(r) {
		_ = tmpl.ExecuteTemplate(w, "sidebar", page)
		return
	}
	_ = renderBase(w, page)
}

type pagerData struct {
	Page    int
	Pages   int
	Total   int
	PrevURL string
	NextURL string
}

func countAgents(flat []*flatItem) []agentCount {
	n := map[string]int{}
	for _, it := range flat {
		n[it.File.Agent]++
	}
	var out []agentCount
	for a, c := range n {
		out = append(out, agentCount{Agent: a, Name: agentDisplayName(a), Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Agent < out[j].Agent
	})
	if len(out) < 2 {
		return nil
	}
	return out
}

func filterByAgent(items []*flatItem, agent string) []*flatItem {
	var out []*flatItem
	for _, it := range items {
		if it.File.Agent == agent {
			out = append(out, it)
		}
	}
	return out
}

func paginateFlat(flat []*flatItem, page int, base, q, agent string) ([]*flatItem, *pagerData) {
	total := len(flat)
	pages := max(1, (total+sessionsPerPage-1)/sessionsPerPage)
	page = min(max(page, 1), pages)
	start := (page - 1) * sessionsPerPage
	end := min(start+sessionsPerPage, total)
	pd := &pagerData{Page: page, Pages: pages, Total: total}
	if page > 1 {
		pd.PrevURL = pageURL(base, q, agent, page-1)
	}
	if page < pages {
		pd.NextURL = pageURL(base, q, agent, page+1)
	}
	return flat[start:end], pd
}

func pageURL(base, q, agent string, page int) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if agent != "" {
		v.Set("agent", agent)
	}
	v.Set("p", strconv.Itoa(page))
	return base + "?" + v.Encode()
}

func pageContaining(flat []*flatItem, rel string) int {
	for i, it := range flat {
		if it.File.Path == rel {
			return i/sessionsPerPage + 1
		}
		for _, a := range it.Agents {
			if a.Path == rel {
				return i/sessionsPerPage + 1
			}
		}
	}
	return 1
}

func filterFlatList(items []*flatItem, q string) []*flatItem {
	q = strings.ToLower(q)
	var out []*flatItem
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.File.Title), q) ||
			strings.Contains(strings.ToLower(item.File.Path), q) ||
			strings.Contains(strings.ToLower(item.Folder), q) ||
			strings.Contains(strings.ToLower(item.File.Host), q) ||
			strings.Contains(item.File.Agent, q) {
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
		events, err := s.events(ctx, rel, ver)
		if err != nil {
			sessionError(w, r, err)
			return
		}
		sess = Normalize(events, nil)
		setAgentPaths(sess, rel)
		sess.Facts = factsFromEvents(events)
		setEntryAgent(sess, agentOfPath(rel))
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

	eventIndex := 0
	if event := r.URL.Query().Get("event"); event != "" {
		var err error
		eventIndex, err = strconv.Atoi(event)
		if err != nil || eventIndex < 1 || eventIndex > len(sess.Entries) {
			http.NotFound(w, r)
			return
		}
	}
	if eventIndex == 0 {
		s.cache.set(rel, fingerprintAll(sess.Entries))
	}

	file := &fileInfo{
		Name:  filepath.Base(rel),
		Path:  rel,
		Agent: agentOfPath(rel),
		Facts: sess.Facts,
	}
	file.IsAgent = isAgentPath(rel)
	if file.IsAgent {
		file.AgentID = subagentLabel(rel)
	}
	if sess.Facts != nil {
		file.Title = sess.Facts.Title
	}
	if file.Title == "" && file.IsAgent {
		file.Title = file.AgentID
	}
	file.Folder = folderFor(rel, sess.Facts)

	flat, err := s.buildSidebar(ctx)
	for _, it := range flat {
		if it.File.Path == rel {
			file.Host = it.File.Host
		}
		for _, a := range it.Agents {
			if a.Path == rel {
				file.Host = a.Host
				file.ParentPath = it.File.Path
				file.IsAgent = true
				file.AgentID = a.AgentID
			}
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hidden := readHidden(r)

	pageNum, _ := strconv.Atoi(r.URL.Query().Get("p"))
	if pageNum < 1 {
		pageNum = pageContaining(flat, rel)
	}
	flat, pager := paginateFlat(flat, pageNum, "/session/"+rel, "", "")

	copySession := *sess
	copySession.Entries = append([]Entry(nil), sess.Entries...)
	sess = &copySession
	if eventIndex == 0 {
		s.decorateVotes(ctx, rel, sess)
	}
	encoder := s.chosenEncoder(ctx, r)
	transient := s.enqueueSae(ctx, r, rel, sess, encoder)
	saeSession := sess
	if eventIndex > 0 {
		saeSession = &Session{Entries: []Entry{sess.Entries[eventIndex-1]}, Facts: sess.Facts}
	}
	saeData := s.decorateSae(ctx, saeSession, encoder)
	applySaeTransient(saeData.Payload, transient)
	setSaeSession(saeData, rel)
	if limit := r.URL.Query().Get("limit"); limit != "" {
		u, _ := url.Parse(saeData.Payload.RefreshURL)
		q := u.Query()
		q.Set("limit", limit)
		u.RawQuery = q.Encode()
		saeData.Payload.RefreshURL = u.String()
		saeData.JSON = saeData.Payload.json()
	}

	page := pageData{
		Title:       "pravda · " + file.Name,
		FlatList:    flat,
		Pager:       pager,
		CurrentPath: rel,
		Session:     sess,
		SessionFile: file,
		Timeline:    timelineFor(r, rel, sess, hidden, childMarkers(flat, rel)),

		Sae:         saeData,
		HiddenKinds: hidden,
		GroupBy:     readGroup(r),
		Preset:      presetOf(hidden),
		JumpLines:   readBool(r, cookieJumpLines, true),
		JumpFile:    readBool(r, cookieJumpFile, true),
	}

	page.Groups = groupItems(page.FlatList, page.GroupBy)
	if eventIndex == 0 {
		page.Env = buildEnv(sess)
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
			BackURL: "/session/" + rel + "?encoder=" + url.QueryEscape(saeData.Encoder),
			Sae:     saeInspector(saeData, entry),
		}
		s.saeInspectorTable(r.Context(), page.Inspector.Sae, saeData.Encoder, r.URL)
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
	case "preset":
		list, ok := presets[r.FormValue("name")]
		if !ok {
			http.Error(w, "unknown preset", http.StatusBadRequest)
			return
		}
		writeStringList(w, cookieHidden, list)
	case "sae-encoder":
		if !s.validEncoder(r.Context(), r.FormValue("encoder")) {
			http.Error(w, "unknown SAE dictionary", http.StatusBadRequest)
			return
		}
		writeCookieString(w, cookieSaeEncoder, r.FormValue("encoder"))
		back, _ := url.Parse(safeBack(r))
		q := back.Query()
		q.Set("encoder", r.FormValue("encoder"))
		back.RawQuery = q.Encode()
		http.Redirect(w, r, back.String(), http.StatusSeeOther)
		return
	case "group":
		switch v := r.FormValue("mode"); v {
		case groupNone, groupProject, groupHost:
			writeCookieString(w, cookieGroup, v)
		default:
			http.Error(w, "unknown group mode", http.StatusBadRequest)
			return
		}
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
		item := &flatItem{File: g.Session, Folder: g.Session.Folder}
		item.Agents = append(item.Agents, g.Agents...)
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

	s.loadFacts(ctx)
	var needTitle []int
	for i := range metas {
		if cached, ok := s.titles.Load(metas[i].Path); ok {
			if f, ok := cached.(*cachedFacts); ok && (f.lines >= headLines || metas[i].LineCount <= f.lines) {
				metas[i].Facts = f.facts
				metas[i].Title = f.facts.Title
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
	byID := map[string]string{}
	for _, m := range metas {
		if m.Facts != nil && m.Facts.SessionID != "" && !isAgentPath(m.Path) {
			byID[m.Facts.SessionID] = m.Path
		}
	}
	sessions := make(map[string]*sessionGroup)
	group := func(p string) *sessionGroup {
		if sessions[p] == nil {
			sessions[p] = &sessionGroup{}
		}
		return sessions[p]
	}
	for _, m := range metas {
		if filepath.Base(m.Path) == "history.jsonl" {
			continue
		}
		fi := metaToFileInfo(m)
		switch {
		case isAgentPath(m.Path):
			fi.ParentPath = parentSessionOf(m.Path)
			group(fi.ParentPath).Agents = append(group(fi.ParentPath).Agents, fi)
		case m.Facts != nil && m.Facts.Parent != "" && byID[m.Facts.Parent] != "" && byID[m.Facts.Parent] != m.Path:
			fi.IsAgent = true
			fi.ParentPath = byID[m.Facts.Parent]
			fi.AgentID = m.Facts.Source
			if m.Facts.Nickname != "" {
				fi.AgentID = m.Facts.Nickname
				if m.Facts.Source != "" && m.Facts.Source != "subagent" {
					fi.AgentID += " · " + m.Facts.Source
				}
			}
			if fi.AgentID == "" {
				fi.AgentID = shortID(m.Facts.SessionID)
			}
			group(fi.ParentPath).Agents = append(group(fi.ParentPath).Agents, fi)
		default:
			group(m.Path).Session = fi
		}
	}
	out := make([]*sessionGroup, 0, len(sessions))
	for path, g := range sessions {
		if g.Session == nil {
			g.Session = &fileInfo{Name: filepath.Base(path), Path: path, Agent: agentOfPath(path), Folder: prettyFolder(filepath.ToSlash(filepath.Dir(path)))}
		}
		sort.Slice(g.Agents, func(i, j int) bool { return g.Agents[i].Modified < g.Agents[j].Modified })
		out = append(out, g)
	}
	return out, nil
}

var fileIDReplacer = strings.NewReplacer("/", "-", ".", "-")

func metaToFileInfo(m SessionMeta) *fileInfo {
	name := filepath.Base(m.Path)
	fi := &fileInfo{
		ID:       fileIDReplacer.Replace(m.Path),
		Name:     name,
		Path:     m.Path,
		Size:     m.Size,
		Modified: m.ModTime.Format(time.RFC3339),
		Title:    m.Title,
		IsAgent:  isAgentPath(m.Path),
		Agent:    agentOfPath(m.Path),
		Host:     m.Host,
		Facts:    m.Facts,
	}
	if fi.IsAgent {
		fi.AgentID = subagentLabel(m.Path)
	}
	fi.Folder = folderFor(m.Path, m.Facts)
	return fi
}

func folderFor(rel string, f *sessionFacts) string {
	if f != nil && f.CWD != "" {
		return prettyCWD(f.CWD)
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	if agentOf(rel) != "" {
		return ""
	}
	if slug := filepath.Base(dir); strings.HasPrefix(slug, "-") {
		return prettyCWD(strings.ReplaceAll(slug, "-", "/"))
	}
	return prettyFolder(dir)
}

func timelineFor(r *http.Request, rel string, sess *Session, hidden []string, markers []Entry) *timelineData {
	td := newTimeline(sess, hidden)
	td.Path = rel
	td.Entries = mergeMarkers(sess.Entries, markers)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			td.Limit = n
		}
	}
	return td
}

func setEntryAgent(sess *Session, agent string) {
	for i := range sess.Entries {
		sess.Entries[i].Agent = agent
		for j := range sess.Entries[i].SubAgent {
			sess.Entries[i].SubAgent[j].Agent = agent
		}
	}
}

func setAgentPaths(sess *Session, parentRel string) {
	base := strings.TrimSuffix(parentRel, filepath.Ext(parentRel))
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.Kind == KindWorkflow {
			e.SubAgentPath = filepath.ToSlash(filepath.Join(filepath.Dir(parentRel), "agent-"+e.AgentID+".jsonl"))
			continue
		}
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
	events, err := s.events(ctx, rel, ver)
	if err != nil {
		sessionError(w, r, err)
		return
	}
	sess := Normalize(events, nil)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "subagent-entries", sess)
}

func sessionError(w http.ResponseWriter, r *http.Request, err error) {
	var nf *pathNotFoundError
	if errors.As(err, &nf) || errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

type sessionHeadOpener interface {
	OpenSessionHead(ctx context.Context, path string, limit int) (io.ReadCloser, error)
}

func (s *WatchServer) extractMissingTitles(ctx context.Context, metas []SessionMeta, indices []int) {
	type result struct {
		idx   int
		facts *sessionFacts
	}
	ch := make(chan result, len(indices))
	sem := make(chan struct{}, 16)
	for _, idx := range indices {
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem }()
			ch <- result{i, s.extractSessionFacts(ctx, metas[i].Path)}
		}(idx)
	}
	for range indices {
		r := <-ch
		if r.facts == nil {
			continue
		}
		metas[r.idx].Facts = r.facts
		metas[r.idx].Title = r.facts.Title
		if r.facts.Title != "" || r.facts.CWD != "" {
			s.titles.Store(metas[r.idx].Path, &cachedFacts{facts: r.facts, lines: metas[r.idx].LineCount})
			if fs, ok := innerStore(s.Store).(factsStore); ok {
				if err := fs.SaveFacts(ctx, metas[r.idx].Path, metas[r.idx].LineCount, r.facts); err != nil {
					log.Printf("save facts %s: %v", metas[r.idx].Path, err)
				}
			}
		}
	}
}

func (s *WatchServer) loadFacts(ctx context.Context) {
	s.factsOnce.Do(func() {
		fs, ok := innerStore(s.Store).(factsStore)
		if !ok {
			return
		}
		rows, err := fs.LoadFacts(ctx)
		if err != nil {
			log.Printf("load facts: %v", err)
			return
		}
		for p, cf := range rows {
			if _, loaded := s.titles.Load(p); !loaded {
				s.titles.Store(p, &cachedFacts{facts: cf.facts, lines: cf.lines})
			}
		}
	})
}

func (s *WatchServer) extractSessionFacts(ctx context.Context, path string) *sessionFacts {
	if ho, ok := s.Store.(sessionHeadOpener); ok {
		rc, err := ho.OpenSessionHead(ctx, path, headLines)
		if err == nil {
			defer rc.Close()
			return factsFromReader(rc)
		}
	}
	rc, err := s.Store.OpenSession(ctx, path)
	if err != nil {
		return nil
	}
	defer rc.Close()
	return factsFromReader(rc)
}

func extractTitleFromEvents(events []*RawEvent) string {
	var custom, ai, prompt string
	for _, e := range events {
		if t := titledEventTitle(e); t != "" {
			if e.Type == "custom-title" {
				custom = t
			} else {
				ai = t
			}
		}
		if prompt == "" {
			prompt = userPromptTitle(e)
		}
	}
	if custom != "" {
		return custom
	}
	if ai != "" {
		return ai
	}
	if prompt != "" {
		return prompt
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

func titledEventTitle(e *RawEvent) string {
	if e == nil {
		return ""
	}
	switch e.Type {
	case "custom-title":
		return firstLineEllipsis(e.CustomTitle, 90)
	case "ai-title":
		return firstLineEllipsis(e.AITitle, 90)
	case "summary":
		return firstLineEllipsis(e.Summary, 90)
	}
	return ""
}

func userPromptTitle(e *RawEvent) string {
	if e == nil || e.Type != "user" || e.IsMeta || e.Message == nil {
		return ""
	}
	var s string
	if items := decodeContent(e.Message.Content); items != nil {
		for _, item := range items {
			if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
				s = item.Text
				break
			}
		}
	} else {
		s = decodeString(e.Message.Content)
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "[{") || strings.HasPrefix(s, "{\"") || strings.HasPrefix(s, "# AGENTS.md") {
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
