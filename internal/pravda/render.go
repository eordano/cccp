package pravda

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

//go:embed templates/*.html templates/**/*.html
var templateFS embed.FS

var md = goldmark.New(goldmark.WithRendererOptions(gmhtml.WithHardWraps()))

var tmpl *template.Template

func init() {
	funcs := template.FuncMap{
		"markdown":     renderMarkdown,
		"formatStamp":  formatStamp,
		"formatClock":  formatClock,
		"formatMs":     formatMs,
		"stripAnsi":    stripAnsi,
		"basename":     basename,
		"json":         jsonIndent,
		"lineCount":    lineCount,
		"firstLine":    firstLine,
		"truncate":     truncate,
		"shortID":      shortID,
		"dict":         dict,
		"resultText":   resultText,
		"decodeString": decodeString,
		"field":        field,
		"rawField":     rawField,
		"rawPath":      rawPath,
		"rawInt":       rawInt,
		"rawList":      rawList,
		"eventData":    eventData,
		"add1":         func(i int) int { return i + 1 },
		"add":          func(a, b int) int { return a + b },
		"sub":          func(a, b int) int { return a - b },
		"renderEntry":  renderEntryContent,
		"formatSize":   formatSize,
		"parseNotif":   parseNotifFromString,
		"filterGroups": filterGroups,
		"kindClass":    func(s string) string { return strings.ReplaceAll(s, ":", "-") },
	}
	tmpl = template.Must(
		template.New("root").Funcs(funcs).ParseFS(templateFS,
			"templates/*.html",
			"templates/events/*.html",
			"templates/tools/*.html",
		))
}

func renderMarkdown(text string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(buf.String())
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripAnsi(s string) string { return ansiRe.ReplaceAllString(s, "") }

func basename(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func jsonIndent(v interface{}) string {
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return "(not available)"
		}
		var tmp interface{}
		if err := json.Unmarshal(raw, &tmp); err != nil {
			return string(raw)
		}
		v = tmp
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func dict(kv ...interface{}) map[string]interface{} {
	if len(kv)%2 != 0 {
		return nil
	}
	m := make(map[string]interface{}, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, _ := kv[i].(string)
		m[k] = kv[i+1]
	}
	return m
}

func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		return decodeString(raw)
	}
	if raw[0] == '[' {
		var items []*ContentItem
		if err := json.Unmarshal(raw, &items); err != nil {
			return string(raw)
		}
		var parts []string
		for _, it := range items {
			switch it.Type {
			case "text":
				parts = append(parts, it.Text)
			case "tool_reference":
				parts = append(parts, "-> "+it.ToolName)
			default:
				b, _ := json.Marshal(it)
				parts = append(parts, string(b))
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func field(e *Entry, key string) string {
	if e.InputMap != nil {
		v, ok := e.InputMap[key]
		if !ok {
			return ""
		}
		return decodeString(v)
	}
	return rawField(e.Input, key)
}

func rawField(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	return decodeString(v)
}

func rawPath(raw json.RawMessage, keys ...string) string {
	cur := raw
	for _, k := range keys {
		if len(cur) == 0 {
			return ""
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(cur, &m); err != nil {
			return ""
		}
		next, ok := m[k]
		if !ok {
			return ""
		}
		cur = next
	}
	return decodeString(cur)
}

func rawInt(raw json.RawMessage, key string) int64 {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	var n int64
	_ = json.Unmarshal(v, &n)
	return n
}

func rawList(raw json.RawMessage, key string) []json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	var out []json.RawMessage
	_ = json.Unmarshal(v, &out)
	return out
}

func eventData(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	return m
}

var notifTagRe = func() map[string]*regexp.Regexp {
	tags := []string{"task-id", "tool-use-id", "status", "summary", "result", "output-file"}
	m := make(map[string]*regexp.Regexp, len(tags))
	for _, t := range tags {
		m[t] = regexp.MustCompile(`(?s)<` + t + `>(.*?)</` + t + `>`)
	}
	return m
}()

func parseNotifFromString(s string) map[string]string {
	if !strings.HasPrefix(strings.TrimSpace(s), "<task-notification>") {
		return nil
	}
	grab := func(tag string) string {
		m := notifTagRe[tag].FindStringSubmatch(s)
		if len(m) < 2 {
			return ""
		}
		return strings.TrimSpace(m[1])
	}
	return map[string]string{
		"taskId":     grab("task-id"),
		"toolUseId":  grab("tool-use-id"),
		"status":     grab("status"),
		"summary":    grab("summary"),
		"result":     grab("result"),
		"outputFile": grab("output-file"),
	}
}

func renderBase(w io.Writer, page pageData) error {
	return tmpl.ExecuteTemplate(w, "base.html", page)
}

func renderEntryContent(e *Entry) template.HTML {
	name := e.TemplateName()
	if tmpl.Lookup(name) == nil {
		if e.Kind == KindTool {
			name = "tool-generic"
		} else {
			name = "event-unknown"
		}
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, e); err != nil {
		return template.HTML("<!-- render error: " + template.HTMLEscapeString(err.Error()) + " -->")
	}
	return template.HTML(buf.String())
}

type pageData struct {
	Title       string
	HiddenKinds []string
	FlatList    []*flatItem
	CurrentPath string
	Session     *Session
	SessionFile *fileInfo
	Inspector   *inspectorData
	JumpLines   bool
	JumpFile    bool
	SearchQuery string
}

type flatItem struct {
	File   *fileInfo
	Folder string
	Agents []*fileInfo
}

type fileInfo struct {
	ID       string
	Path     string
	Name     string
	Size     int64
	Modified string
	Title    string
	IsAgent  bool
	AgentID  string
}

type folderNode struct {
	Folder   string
	Label    string
	Sessions []*sessionGroup
}

type sessionGroup struct {
	Session *fileInfo
	Agents  []*fileInfo
}

type inspectorData struct {
	Entry   *Entry
	Raw     *RawEvent
	Result  *RawEvent
	ToolUse *ContentItem
	BackURL string
}

type filterGroup struct {
	Label string
	Kinds []filterKind
}
type filterKind struct {
	Key    string
	Label  string
	Hidden bool
}

func filterGroups(hidden []string) []filterGroup {
	hiddenSet := map[string]bool{}
	for _, h := range hidden {
		hiddenSet[h] = true
	}
	k := func(key, label string) filterKind {
		return filterKind{Key: key, Label: label, Hidden: hiddenSet[key]}
	}
	return []filterGroup{
		{Label: "Messaging", Kinds: []filterKind{
			k("user", "User"), k("message", "Assistant"),
			k("interrupt", "Interrupt"), k("meta", "Meta"),
			k("command", "Slash command"), k("command-output", "Command output"),
		}},
		{Label: "Thinking", Kinds: []filterKind{
			k("thinking", "Thinking"),
		}},
		{Label: "System", Kinds: []filterKind{
			k("turn_duration", "Turn duration"),
			k("permission-mode", "Permission mode"),
			k("queue-operation", "Queue op"),
			k("system", "System"),
		}},
		{Label: "Attachments", Kinds: []filterKind{
			k("attachment:file", "file"),
			k("attachment:compact_file_reference", "compact_file_reference"),
			k("attachment:directory", "directory"),
			k("attachment:plan_mode_exit", "plan_mode_exit"),
			k("attachment:queued_command", "queued_command"),
			k("attachment:task_reminder", "task_reminder"),
			k("attachment:skill_listing", "skill_listing"),
			k("attachment:deferred_tools_delta", "deferred_tools_delta"),
		}},
	}
}
