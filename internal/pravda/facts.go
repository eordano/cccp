package pravda

import (
	"bufio"
	"encoding/json"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/eordano/cccp/internal/syncclass"
)

type sessionFacts struct {
	Title     string
	CWD       string
	Model     string
	Version   string
	GitBranch string
	SessionID string
	Parent    string
	Source    string
	Nickname  string
	Started   string
}

const headLines = 60

var agentNames = map[string]string{
	"":         "Claude",
	"claude":   "Claude",
	"codex":    "Codex",
	"hermes":   "Hermes",
	"opencode": "OpenCode",
}

func agentDisplayName(agent string) string {
	if n, ok := agentNames[agent]; ok {
		return n
	}
	if kind := agentKind(agent); kind != agent {
		return agentDisplayName(kind) + " · " + strings.TrimPrefix(agent, kind+"-")
	}
	return strings.ToUpper(agent[:1]) + agent[1:]
}

func agentKind(agent string) string {
	if kind := syncclass.Kind(agent); kind != "" {
		return kind
	}
	return agent
}

func agentOfPath(rel string) string {
	if a := agentOf(rel); a != "" {
		return a
	}
	return "claude"
}

var subagentRe = regexp.MustCompile(`^(.+?)/([^/]+?)/subagents/(.+)\.jsonl$`)

func isAgentPath(rel string) bool { return subagentRe.MatchString(rel) }

func parentSessionOf(rel string) string {
	m := subagentRe.FindStringSubmatch(path.Clean(rel))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2] + ".jsonl"
}

func subagentLabel(rel string) string {
	m := subagentRe.FindStringSubmatch(rel)
	if m == nil {
		return strings.TrimSuffix(path.Base(rel), ".jsonl")
	}
	tail := m[3]
	tail = strings.TrimPrefix(tail, "workflows/")
	parts := strings.Split(tail, "/")
	for i, p := range parts {
		parts[i] = strings.TrimPrefix(p, "agent-")
	}
	return strings.Join(parts, " › ")
}

func prettyCWD(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	if cwd == "" {
		return ""
	}
	if i := strings.Index(cwd, "/home/"); i == 0 {
		rest := strings.SplitN(cwd[len("/home/"):], "/", 2)
		if len(rest) == 2 {
			cwd = "~" + rest[0] + "/" + rest[1]
		} else {
			cwd = "~" + rest[0]
		}
	} else if strings.HasPrefix(cwd, "/Users/") {
		rest := strings.SplitN(cwd[len("/Users/"):], "/", 2)
		if len(rest) == 2 {
			cwd = "~" + rest[0] + "/" + rest[1]
		} else {
			cwd = "~" + rest[0]
		}
	}
	segs := strings.Split(cwd, "/")
	if len(segs) > 3 {
		segs = append([]string{segs[0], "…"}, segs[len(segs)-2:]...)
	}
	return strings.Join(segs, "/")
}

func (f *sessionFacts) absorb(e *RawEvent) {
	if f.Started == "" && e.Timestamp != "" {
		f.Started = e.Timestamp
	}
	if f.CWD == "" && e.CWD != "" {
		f.CWD = e.CWD
	}
	if f.Version == "" && e.Version != "" {
		f.Version = e.Version
	}
	if f.GitBranch == "" && e.GitBranch != "" {
		f.GitBranch = e.GitBranch
	}
	if f.SessionID == "" && e.SessionID != "" {
		f.SessionID = e.SessionID
	}
	if e.Type == "session-meta" {
		if f.Parent == "" {
			f.Parent = e.ParentSessionID
		}
		if f.Source == "" {
			f.Source = e.AgentSource
		}
		if f.Nickname == "" {
			f.Nickname = e.AgentNickname
		}
	}
	if f.Model == "" && e.Message != nil && e.Message.Model != "" {
		f.Model = e.Message.Model
	}
}

func factsFromEvents(events []*RawEvent) *sessionFacts {
	f := &sessionFacts{}
	for _, e := range events {
		f.absorb(e)
	}
	f.Title = extractTitleFromEvents(events)
	return f
}

func factsFromReader(r io.Reader) *sessionFacts {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	f := &sessionFacts{}
	var events []*RawEvent
	for sc.Scan() && len(events) < headLines {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e RawEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		f.absorb(&e)
		events = append(events, &e)
	}
	f.Title = extractTitleFromEvents(events)
	return f
}

func (f *sessionFacts) Facts() []string {
	if f == nil {
		return nil
	}
	var out []string
	if f.CWD != "" {
		out = append(out, prettyCWD(f.CWD))
	}
	if f.Model != "" {
		out = append(out, f.Model)
	}
	if f.GitBranch != "" {
		out = append(out, "⎇ "+f.GitBranch)
	}
	if f.Version != "" {
		v := f.Version
		if v[0] >= '0' && v[0] <= '9' {
			v = "v" + v
		}
		out = append(out, v)
	}
	if f.Source != "" {
		out = append(out, f.Source)
	}
	return out
}

var previewKeys = []string{"command", "cmd", "query", "pattern", "path", "file_path", "filePath", "url", "prompt", "description", "skill", "action", "subject", "task_id", "to", "recipient", "title", "files", "input", "code", "text", "content", "message"}

func toolPreview(e *Entry) string {
	if e == nil {
		return ""
	}
	for _, k := range previewKeys {
		raw, ok := e.InputMap[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			s = strings.TrimSpace(s)
			if s != "" {
				return firstLineEllipsis(s, 120)
			}
			continue
		}
		return firstLineEllipsis(string(raw), 120)
	}
	if len(e.Input) > 0 && string(e.Input) != "{}" && string(e.Input) != "null" {
		var s string
		if err := json.Unmarshal(e.Input, &s); err == nil {
			return firstLineEllipsis(strings.TrimSpace(s), 120)
		}
		return firstLineEllipsis(string(e.Input), 120)
	}
	return ""
}

type timelineData struct {
	Entries []Entry
	Hidden  map[string]bool
	Limit   int
	Path    string

	Total   int
	Shown   int
	Skipped int
}

const timelineWindow = 400

func hiddenKey(e *Entry) string {
	switch e.Kind {
	case KindAttachment:
		return "attachment:" + e.Subtype
	}
	return e.Kind
}

func newTimeline(sess *Session, hidden []string) *timelineData {
	td := &timelineData{Entries: sess.Entries, Limit: timelineWindow}
	if len(hidden) > 0 {
		td.Hidden = make(map[string]bool, len(hidden))
		for _, h := range hidden {
			td.Hidden[h] = true
		}
	}
	return td
}

func (t *timelineData) visible() []*Entry {
	out := make([]*Entry, 0, len(t.Entries))
	for i := range t.Entries {
		e := &t.Entries[i]
		if t.Hidden[hiddenKey(e)] || (e.Kind == KindAttachment && t.Hidden["attachment"]) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (t *timelineData) Visible() []*Entry {
	all := t.visible()
	t.Total = len(all)
	t.Skipped = 0
	if t.Limit > 0 && len(all) > t.Limit {
		t.Skipped = len(all) - t.Limit
		all = all[t.Skipped:]
	}
	t.Shown = len(all)
	return all
}

func (t *timelineData) EarlierURL() string {
	if t.Skipped == 0 {
		return ""
	}
	return "/session/" + t.Path + "?limit=" + strconv.Itoa(t.Limit+timelineWindow) + "#e-" + strconv.Itoa(t.Entries[len(t.Entries)-t.Shown].Index)
}

func (t *timelineData) AllURL() string {
	if t.Skipped == 0 {
		return ""
	}
	return "/session/" + t.Path + "?limit=0"
}

const (
	timelineTextCap = 16 * 1024
	timelineJSONCap = 16 * 1024
)

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… " + humanBytes(len(s)-cut) + " more, open the inspector for the full text"
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	case n >= 1<<10:
		return strconv.Itoa(n>>10) + " KiB"
	}
	return strconv.Itoa(n) + " B"
}

type sidebarGroup struct {
	Key   string
	Label string
	Items []*flatItem
}

func extraFuncs() map[string]any {
	return map[string]any{
		"fmtTokens": fmtTokens,
		"fmtUSD":    fmtUSD,
	}
}
