package pravda

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/api"
	"github.com/eordano/cccp/internal/store"
	"github.com/eordano/cccp/internal/syncclass"
)

const IndexVersion = 1

const (
	rebuildChunk    = 256
	followBatch     = 2000
	followBatchSize = 64 << 20
	maxListedNames  = 256
	recentMsgIDs    = 16
)

type foldTokens struct {
	Input         int64   `json:"i,omitempty"`
	Output        int64   `json:"o,omitempty"`
	CacheRead     int64   `json:"cr,omitempty"`
	CacheCreation int64   `json:"cc,omitempty"`
	Cost          float64 `json:"c,omitempty"`
}

type bgRef struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
}

type foldState struct {
	Host      string `json:"host,omitempty"`
	SessionID string `json:"sid,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	GitBranch string `json:"branch,omitempty"`
	GitCommit string `json:"commit,omitempty"`
	GitRepo   string `json:"repo,omitempty"`
	Version   string `json:"ver,omitempty"`
	User      string `json:"user,omitempty"`
	Parent    string `json:"parent,omitempty"`
	Source    string `json:"source,omitempty"`

	Custom   string `json:"custom,omitempty"`
	CustomTS int64  `json:"custom_ts,omitempty"`
	AI       string `json:"ai,omitempty"`
	AITS     int64  `json:"ai_ts,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	PromptTS int64  `json:"prompt_ts,omitempty"`
	Reply    string `json:"reply,omitempty"`
	ReplyTS  int64  `json:"reply_ts,omitempty"`

	Models    []string `json:"models,omitempty"`
	LastModel string   `json:"last_model,omitempty"`

	Start     int64 `json:"start,omitempty"`
	End       int64 `json:"end,omitempty"`
	Lines     int   `json:"lines,omitempty"`
	Turns     int   `json:"turns,omitempty"`
	Requests  int   `json:"requests,omitempty"`
	ToolCalls int   `json:"tools,omitempty"`
	Errors    int   `json:"errors,omitempty"`

	Tokens     map[string]*foldTokens `json:"tokens,omitempty"`
	Reported   map[string]*foldTokens `json:"reported,omitempty"`
	ReportedTS int64                  `json:"reported_ts,omitempty"`
	ReportedC  float64                `json:"reported_cost,omitempty"`
	RecentMsgs []string               `json:"recent,omitempty"`
	LastUsage  string                 `json:"last_usage,omitempty"`

	Instructions []bgRef  `json:"instructions,omitempty"`
	Skills       []string `json:"skills,omitempty"`
	Invoked      []string `json:"invoked,omitempty"`
}

type lineExtras struct {
	Message *struct {
		ID string `json:"id"`
	} `json:"message"`
	RequestID    string          `json:"requestId"`
	IsAPIError   bool            `json:"isApiErrorMessage"`
	Error        json.RawMessage `json:"error"`
	GitCommit    string          `json:"gitCommit"`
	GitRepo      string          `json:"gitRepo"`
	User         string          `json:"user"`
	TotalCostUSD float64         `json:"totalCostUSD"`
}

func indexable(p string) bool {
	if !strings.HasSuffix(p, ".jsonl") || path.Base(p) == "history.jsonl" {
		return false
	}
	if agentOf(p) == "" {
		return strings.HasPrefix(p, "projects/") && !strings.HasPrefix(p, "agent/")
	}
	return isListedSession(p)
}

func parseNS(ts string) int64 {
	if ts == "" {
		return 0
	}
	if t, ok := parseTS(ts); ok {
		return t.UnixNano()
	}
	return 0
}

func (f *foldState) addModel(m string) {
	if m == "" || m == "<synthetic>" {
		return
	}
	f.LastModel = m
	for _, have := range f.Models {
		if have == m {
			return
		}
	}
	f.Models = append(f.Models, m)
}

func addName(list []string, v string) []string {
	if v == "" || len(list) >= maxListedNames {
		return list
	}
	for _, have := range list {
		if have == v {
			return list
		}
	}
	return append(list, v)
}

func (f *foldState) tokens(model string) *foldTokens {
	if model == "" {
		model = f.LastModel
	}
	if model == "" {
		model = "unknown"
	}
	if f.Tokens == nil {
		f.Tokens = map[string]*foldTokens{}
	}
	t := f.Tokens[model]
	if t == nil {
		t = &foldTokens{}
		f.Tokens[model] = t
	}
	return t
}

func (f *foldState) seenMsg(id string) bool {
	for _, have := range f.RecentMsgs {
		if have == id {
			return true
		}
	}
	f.RecentMsgs = append(f.RecentMsgs, id)
	if len(f.RecentMsgs) > recentMsgIDs {
		f.RecentMsgs = f.RecentMsgs[len(f.RecentMsgs)-recentMsgIDs:]
	}
	return false
}

func homeUser(p string) string {
	for _, pre := range []string{"/home/", "/Users/"} {
		if rest, ok := strings.CutPrefix(p, pre); ok {
			if i := strings.IndexByte(rest, '/'); i > 0 {
				return rest[:i]
			}
			return rest
		}
	}
	if p == "/root" || strings.HasPrefix(p, "/root/") {
		return "root"
	}
	return ""
}

func wirePathOf(p, cwd string) string {
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		if cwd == "" {
			return ""
		}
		p = path.Join(cwd, p)
	}
	p = path.Clean(p)
	for _, h := range []struct{ marker, prefix string }{
		{"/.claude/", ""},
		{"/.codex/", "agent/codex/"},
		{"/.hermes/", "agent/hermes/"},
		{"/.local/share/opencode/", "agent/opencode/"},
	} {
		if i := strings.LastIndex(p, h.marker); i >= 0 {
			rest := p[i+len(h.marker):]
			if rest == "" {
				return ""
			}
			return h.prefix + rest
		}
	}
	return ""
}

func inSpace(wire, agent string) string {
	kind := syncclass.Kind(agent)
	if kind == "" || kind == agent {
		return wire
	}
	if rest, ok := strings.CutPrefix(wire, "agent/"+kind+"/"); ok {
		return "agent/" + agent + "/" + rest
	}
	return wire
}

var writeTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true, "write": true, "edit": true, "patch": true}

func writtenFiles(name string, input map[string]json.RawMessage) []string {
	if !writeTools[name] {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "notebook_path", "filePath", "path"} {
		var s string
		if json.Unmarshal(input[k], &s) == nil && s != "" {
			out = append(out, s)
			break
		}
	}
	var patch string
	if json.Unmarshal(input["patch"], &patch) == nil && patch != "" {
		for _, m := range codexPatchFileRe.FindAllStringSubmatch(patch, -1) {
			out = append(out, strings.TrimSpace(m[2]))
		}
	}
	return out
}

func (f *foldState) fold(e *RawEvent, line store.LogLine, sessionPath string) []store.SessionWrite {
	if f.Host == "" {
		f.Host = line.FirstHost
	}
	ts := parseNS(e.Timestamp)
	if ts == 0 {
		ts = line.SuggestedTime
	}
	if ts > 0 && e.Type != "usage" {
		if f.Start == 0 || ts < f.Start {
			f.Start = ts
		}
		if ts > f.End {
			f.End = ts
		}
	}
	if f.SessionID == "" && e.SessionID != "" {
		f.SessionID = e.SessionID
	}
	if f.CWD == "" && e.CWD != "" {
		f.CWD = e.CWD
	}
	if f.GitBranch == "" && e.GitBranch != "" && e.GitBranch != "HEAD" {
		f.GitBranch = e.GitBranch
	}
	if f.Version == "" && e.Version != "" {
		f.Version = e.Version
	}
	var x lineExtras
	switch e.Type {
	case "assistant", "session-meta", "cost-state", "system":
		_ = json.Unmarshal(e.Raw, &x)
	}
	var writes []store.SessionWrite
	switch e.Type {
	case "session-meta":
		if e.SessionID != "" && f.SessionID != e.SessionID && f.Parent == "" {
			f.SessionID = e.SessionID
		}
		if f.Parent == "" {
			f.Parent = e.ParentSessionID
		}
		if f.Source == "" {
			f.Source = e.AgentSource
		}
		if f.GitCommit == "" {
			f.GitCommit = x.GitCommit
		}
		if f.GitRepo == "" {
			f.GitRepo = x.GitRepo
		}
		if f.User == "" {
			f.User = x.User
		}
		f.addModel(e.Model)
	case "model-change":
		f.addModel(e.Model)
	case "custom-title":
		if t := titledEventTitle(e); t != "" && ts >= f.CustomTS {
			f.Custom, f.CustomTS = t, ts
		}
	case "ai-title", "summary":
		if t := titledEventTitle(e); t != "" && ts >= f.AITS {
			f.AI, f.AITS = t, ts
		}
	case "user":
		if t := userPromptTitle(e); t != "" {
			f.Turns++
			if f.Prompt == "" || (ts > 0 && ts < f.PromptTS) {
				f.Prompt, f.PromptTS = t, ts
			}
		}
		if e.Message != nil {
			for _, item := range decodeContent(e.Message.Content) {
				if item.Type == "tool_result" && item.IsError {
					f.Errors++
				}
			}
		}
	case "assistant":
		if e.Message == nil {
			break
		}
		if x.IsAPIError || len(x.Error) > 0 && string(x.Error) != "null" {
			f.Errors++
		}
		f.addModel(e.Message.Model)
		id := ""
		if x.Message != nil {
			id = x.Message.ID
		}
		if id == "" {
			id = x.RequestID
		}
		fresh := id == "" || !f.seenMsg(id)
		if fresh && e.Message.Model != "<synthetic>" {
			f.Requests++
			if u := e.Message.Usage; u != nil {
				t := f.tokens(e.Message.Model)
				t.Input += u.Input
				t.Output += u.Output
				t.CacheRead += u.CacheRead
				t.CacheCreation += u.CacheCreation
			}
		}
		for _, item := range decodeContent(e.Message.Content) {
			switch item.Type {
			case "text":
				if f.Reply == "" || (ts > 0 && ts < f.ReplyTS) {
					if t := firstLineEllipsis(item.Text, 90); t != "" {
						f.Reply, f.ReplyTS = t, ts
					}
				}
			case "tool_use":
				f.ToolCalls++
				var input map[string]json.RawMessage
				_ = json.Unmarshal(item.Input, &input)
				if item.Name == "Skill" {
					var skill string
					_ = json.Unmarshal(input["skill"], &skill)
					f.Invoked = addName(f.Invoked, skill)
				}
				for _, fp := range writtenFiles(item.Name, input) {
					if wire := inSpace(wirePathOf(fp, f.CWD), agentOf(sessionPath)); wire != "" {
						writes = append(writes, store.SessionWrite{FilePath: wire, SessionPath: sessionPath, TS: ts, Host: line.FirstHost, Tool: item.Name})
					}
				}
			}
		}
	case "usage":
		if e.Usage == nil && e.CostUSD == 0 {
			break
		}
		key, _ := json.Marshal([]any{e.Model, e.Usage, e.CostUSD})
		if string(key) == f.LastUsage {
			break
		}
		f.LastUsage = string(key)
		t := f.tokens(e.Model)
		if u := e.Usage; u != nil {
			t.Input += u.Input
			t.Output += u.Output
			t.CacheRead += u.CacheRead
			t.CacheCreation += u.CacheCreation
		}
		t.Cost += e.CostUSD
	case "cost-state":
		var cs costState
		if json.Unmarshal(e.Raw, &cs) != nil || (cs.TotalCostUSD == 0 && len(cs.ModelUsage) == 0) || line.FirstSeen < f.ReportedTS {
			break
		}
		f.ReportedTS, f.ReportedC = line.FirstSeen, cs.TotalCostUSD
		f.Reported = map[string]*foldTokens{}
		for m, u := range cs.ModelUsage {
			f.Reported[m] = &foldTokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation, Cost: u.Cost}
			f.addModel(m)
		}
	case "system":
		if e.Subtype == "turn_duration" && len(x.Error) > 0 && string(x.Error) != "null" && string(x.Error) != `""` {
			f.Errors++
		}
		if e.Subtype == "patch" {
			var p struct {
				Files []string `json:"files"`
			}
			if json.Unmarshal(e.Raw, &p) == nil {
				for _, fp := range p.Files {
					if wire := inSpace(wirePathOf(fp, f.CWD), agentOf(sessionPath)); wire != "" {
						writes = append(writes, store.SessionWrite{FilePath: wire, SessionPath: sessionPath, TS: ts, Host: line.FirstHost, Tool: "patch"})
					}
				}
			}
		}
	case "attachment":
		var a struct {
			Type  string   `json:"type"`
			Names []string `json:"names"`
			Path  string   `json:"path"`
			Files []bgRef  `json:"files"`
		}
		if json.Unmarshal(e.Attachment, &a) != nil {
			break
		}
		switch a.Type {
		case "instructions":
			for _, file := range a.Files {
				f.addInstruction(file)
			}
		case "nested_memory":
			f.addInstruction(bgRef{Path: a.Path, Type: "Nested"})
		case "skill_listing":
			for _, n := range a.Names {
				f.Skills = addName(f.Skills, n)
			}
		}
	}
	if f.User == "" {
		f.User = homeUser(f.CWD)
	}
	return writes
}

func (f *foldState) addInstruction(r bgRef) {
	if r.Path == "" || len(f.Instructions) >= maxListedNames {
		return
	}
	for _, have := range f.Instructions {
		if have.Path == r.Path {
			return
		}
	}
	f.Instructions = append(f.Instructions, r)
	if f.User == "" {
		f.User = homeUser(r.Path)
	}
}

func (f *foldState) title() string {
	for _, t := range []string{f.Custom, f.AI, f.Prompt, f.Reply, f.LastModel} {
		if t != "" {
			return t
		}
	}
	return ""
}

func pathStem(p string) string { return strings.TrimSuffix(path.Base(p), ".jsonl") }

func sessionKeys(p string, f *foldState) (id, parentPath, project string) {
	if m := subagentRe.FindStringSubmatch(p); m != nil {
		return m[2] + "/" + m[3], parentSessionOf(p), f.CWD
	}
	id = pathStem(p)
	if syncclass.Kind(agentOf(p)) == "codex" && f.SessionID != "" {
		id = f.SessionID
	}
	project = f.CWD
	if project == "" && agentOf(p) == "" {
		if slug := path.Base(path.Dir(p)); strings.HasPrefix(slug, "-") {
			project = strings.ReplaceAll(slug, "-", "/")
		}
	}
	return id, "", project
}

func (f *foldState) project(p string, cursor int64) store.SessionRow {
	id, parentPath, project := sessionKeys(p, f)
	r := store.SessionRow{
		Path: p, ID: id, SessionID: f.SessionID, Agent: agentOfPath(p), Host: f.Host, User: f.User,
		CWD: f.CWD, Project: project, GitBranch: f.GitBranch, GitCommit: f.GitCommit, Version: f.Version,
		Models: append([]string{}, f.Models...), StartedAt: f.Start, EndedAt: f.End, Turns: f.Turns,
		Requests: f.Requests, ToolCalls: f.ToolCalls, Errors: f.Errors, ParentPath: parentPath,
		Source: f.Source, Title: f.title(), FirstPrompt: f.Prompt, LineCount: f.Lines, Cursor: cursor,
	}
	if parentPath == "" && f.Parent != "" && f.Parent != f.SessionID {
		r.ParentSessionID = f.Parent
	}
	if parentPath != "" && r.Source == "" {
		r.Source = "subagent"
	}
	for _, t := range f.totals() {
		r.InputTokens += t.Input
		r.OutputTokens += t.Output
		r.CacheRead += t.CacheRead
		r.CacheCreation += t.CacheCreation
		r.CostUSD += t.Cost
	}
	if f.ReportedTS > 0 {
		r.CostReported = true
		if f.ReportedC > 0 {
			r.CostUSD = f.ReportedC
		}
	}
	r.State, _ = json.Marshal(f)
	return r
}

func (f *foldState) totals() map[string]foldTokens {
	out := map[string]foldTokens{}
	for m, t := range f.Tokens {
		out[m] = *t
	}
	for m, t := range f.Reported {
		out[m] = *t
	}
	return out
}

func foldLines(row *store.SessionRow, p string, lines []store.LogLine) (store.SessionRow, []store.SessionWrite) {
	f := &foldState{}
	cursor := int64(0)
	if row != nil {
		_ = json.Unmarshal(row.State, f)
		cursor = row.Cursor
	}
	conv := converterFor(p)
	var writes []store.SessionWrite
	for _, l := range lines {
		if l.FirstSeen <= cursor {
			continue
		}
		cursor = l.FirstSeen
		f.Lines++
		raws := [][]byte{l.Contents}
		if conv != nil {
			raws = conv(l.Contents)
		}
		for _, raw := range raws {
			var e RawEvent
			if json.Unmarshal(raw, &e) != nil {
				continue
			}
			e.Raw = raw
			writes = append(writes, f.fold(&e, l, p)...)
		}
	}
	return f.project(p, cursor), writes
}

type Indexer struct {
	Store *store.Store
	Log   *slog.Logger
}

func (ix *Indexer) log() *slog.Logger {
	if ix.Log != nil {
		return ix.Log
	}
	return slog.Default()
}

func (ix *Indexer) Ensure(ctx context.Context) (store.IndexMeta, error) {
	m, ok, err := ix.Store.IndexMeta(ctx)
	if err != nil {
		return m, err
	}
	if ok && m.Version == IndexVersion {
		return m, nil
	}
	ix.log().Info("session index: starting rebuild", "had_version", m.Version, "version", IndexVersion)
	return ix.Store.ResetIndex(ctx, IndexVersion)
}

type RebuildProgress struct {
	Paths   int
	Lines   int
	Done    string
	Elapsed time.Duration
}

func (ix *Indexer) Rebuild(ctx context.Context, reset bool, progress func(RebuildProgress)) (RebuildProgress, error) {
	var m store.IndexMeta
	var err error
	if reset {
		m, err = ix.Store.ResetIndex(ctx, IndexVersion)
	} else {
		m, err = ix.Ensure(ctx)
	}
	if err != nil {
		return RebuildProgress{}, err
	}
	start := time.Now()
	var pr RebuildProgress
	if !m.RebuildActive {
		return pr, nil
	}
	after := m.RebuildPath
	lastReport := time.Now()
	for {
		paths, err := ix.Store.LogPathsAfter(ctx, after, 500)
		if err != nil {
			return pr, err
		}
		if len(paths) == 0 {
			break
		}
		for _, p := range paths {
			if err := ctx.Err(); err != nil {
				return pr, err
			}
			after = p
			if !indexable(p) {
				continue
			}
			n, err := ix.rebuildPath(ctx, m, p)
			if errors.Is(err, store.ErrIndexConflict) {
				cur, _, merr := ix.Store.IndexMeta(ctx)
				if merr != nil {
					return pr, merr
				}
				if cur.Gen != m.Gen || !cur.RebuildActive {
					return pr, nil
				}
				n, err = ix.rebuildPath(ctx, m, p)
			}
			if err != nil {
				return pr, err
			}
			pr.Paths++
			pr.Lines += n
			pr.Done = p
			if progress != nil && time.Since(lastReport) > 5*time.Second {
				pr.Elapsed = time.Since(start)
				progress(pr)
				lastReport = time.Now()
			}
		}
	}
	if err := ix.Store.FinishRebuild(ctx, m); err != nil && !errors.Is(err, store.ErrIndexConflict) {
		return pr, err
	}
	pr.Elapsed = time.Since(start)
	return pr, nil
}

func (ix *Indexer) rebuildPath(ctx context.Context, m store.IndexMeta, p string) (int, error) {
	states, err := ix.Store.SessionStates(ctx, []string{p})
	if err != nil {
		return 0, err
	}
	row := states[p]
	folded := 0
	for {
		after := int64(0)
		expected := int64(-1)
		if row != nil {
			after, expected = row.Cursor, row.Cursor
		}
		lines, err := ix.Store.PathLinesBetween(ctx, p, after, m.RebuildHead, rebuildChunk)
		if err != nil {
			return folded, err
		}
		done := len(lines) < rebuildChunk
		if len(lines) == 0 {
			return folded, ix.Store.CommitRebuild(ctx, m, nil, nil, p)
		}
		next, writes := foldLines(row, p, lines)
		donePath := ""
		if done {
			donePath = p
		}
		if err := ix.Store.CommitRebuild(ctx, m, &store.IndexUpdate{Row: next, Expected: expected}, writes, donePath); err != nil {
			return folded, err
		}
		folded += len(lines)
		row = &next
		if done {
			return folded, nil
		}
	}
}

func (ix *Indexer) CatchUp(ctx context.Context) (int, error) {
	m, err := ix.Ensure(ctx)
	if err != nil {
		return 0, err
	}
	if m.RebuildActive {
		if _, err := ix.Rebuild(ctx, false, func(p RebuildProgress) {
			ix.log().Info("session index: rebuilding", "paths", p.Paths, "lines", p.Lines, "at", p.Done, "elapsed", p.Elapsed.Round(time.Second))
		}); err != nil {
			return 0, err
		}
	}
	total := 0
	for attempt := 0; ; {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		m, _, err = ix.Store.IndexMeta(ctx)
		if err != nil {
			return total, err
		}
		if m.RebuildActive {
			return total, nil
		}
		lines, err := ix.Store.LinesAfter(ctx, m.Cursor, followBatch, followBatchSize)
		if err != nil {
			return total, err
		}
		if len(lines) == 0 {
			return total, nil
		}
		err = ix.followBatch(ctx, m, lines)
		if errors.Is(err, store.ErrIndexConflict) && attempt < 5 {
			attempt++
			continue
		}
		if err != nil {
			return total, err
		}
		attempt = 0
		total += len(lines)
	}
}

func (ix *Indexer) followBatch(ctx context.Context, m store.IndexMeta, lines []store.LogLine) error {
	byPath := map[string][]store.LogLine{}
	var order []string
	for _, l := range lines {
		if !indexable(l.Path) {
			continue
		}
		if _, ok := byPath[l.Path]; !ok {
			order = append(order, l.Path)
		}
		byPath[l.Path] = append(byPath[l.Path], l)
	}
	sort.Strings(order)
	states, err := ix.Store.SessionStates(ctx, order)
	if err != nil {
		return err
	}
	var updates []store.IndexUpdate
	var writes []store.SessionWrite
	for _, p := range order {
		row := states[p]
		expected := int64(-1)
		if row != nil {
			expected = row.Cursor
		}
		next, w := foldLines(row, p, byPath[p])
		if row != nil && next.Cursor == row.Cursor {
			continue
		}
		updates = append(updates, store.IndexUpdate{Row: next, Expected: expected})
		writes = append(writes, w...)
	}
	return ix.Store.CommitFollow(ctx, m, updates, writes, lines[len(lines)-1].FirstSeen)
}

func (ix *Indexer) Follow(ctx context.Context, events *api.Broadcaster) {
	wake := make(chan struct{}, 1)
	if events != nil {
		sub := events.Subscribe()
		defer events.Unsubscribe(sub)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-sub:
					if !ok {
						return
					}
					if ev.Kind == "log" {
						select {
						case wake <- struct{}{}:
						default:
						}
					}
				}
			}
		}()
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		start := time.Now()
		n, err := ix.CatchUp(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			ix.log().Warn("session index", "err", err)
		case n > 0:
			ix.log().Debug("session index: folded", "lines", n, "took", time.Since(start).Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-wake:
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}
