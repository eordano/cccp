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
		"markdown":       func(s string) template.HTML { return renderMarkdown(clipText(s, timelineTextCap)) },
		"markdownFull":   renderMarkdown,
		"formatStamp":    formatStamp,
		"formatClock":    formatClock,
		"formatMs":       formatMs,
		"stripAnsi":      stripAnsi,
		"basename":       basename,
		"json":           func(v interface{}) string { return clipText(jsonIndent(v), timelineJSONCap) },
		"jsonFull":       jsonIndent,
		"lineCount":      lineCount,
		"firstLine":      firstLine,
		"truncate":       truncate,
		"shortID":        shortID,
		"agentName":      agentDisplayName,
		"parentSession":  parentSessionOf,
		"toolPreview":    toolPreview,
		"dict":           dict,
		"resultText":     func(raw json.RawMessage) string { return clipText(resultText(raw), timelineTextCap) },
		"resultTextFull": resultText,
		"decodeString":   decodeString,
		"field":          field,
		"rawField":       rawField,
		"rawPath":        rawPath,
		"rawInt":         rawInt,
		"rawList":        rawList,
		"eventData":      eventData,
		"add1":           func(i int) int { return i + 1 },
		"add":            func(a, b int) int { return a + b },
		"sub":            func(a, b int) int { return a - b },
		"renderEntry":    renderEntryContent,
		"formatSize":     formatSize,
		"formatCount":    formatCount,
		"parseNotif":     parseNotifFromString,
		"filterGroups":   filterGroups,
		"kindClass":      func(s string) string { return strings.ReplaceAll(s, ":", "-") },
	}
	for k, v := range extraFuncs() {
		funcs[k] = v
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
			case "image":
				parts = append(parts, "[image]")
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
	name := "event-unknown"
	for _, candidate := range e.TemplateNames() {
		if tmpl.Lookup(candidate) != nil {
			name = candidate
			break
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
	Pager       *pagerData
	CurrentPath string
	Session     *Session
	SessionFile *fileInfo
	Inspector   *inspectorData
	JumpLines   bool
	JumpFile    bool
	SearchQuery string
	AgentFilter string
	AgentCounts []agentCount
	Timeline    *timelineData
	Env         *sessionEnv
	Sae         *saeSessionData
	GroupBy     string
	Groups      []*sidebarGroup
	Preset      string
}

type agentCount struct {
	Agent string
	Name  string
	Count int
}

type flatItem struct {
	File   *fileInfo
	Folder string
	Agents []*fileInfo
}

type fileInfo struct {
	ID         string
	Path       string
	Name       string
	Size       int64
	Modified   string
	Title      string
	IsAgent    bool
	AgentID    string
	Agent      string
	Host       string
	Folder     string
	ParentPath string
	Facts      *sessionFacts
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
	Sae     *saeInspectorData
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
			k("agent-result", "Sub-agent result"), k("child-session", "Sub-session"),
			k("workflow", "Workflow event"), k("unknown", "Unknown type"),
			k("command", "Slash command"), k("command-output", "Command output"),
			k("bash-input", "! bash input"), k("bash-output", "! bash output"),
		}},
		{Label: "Thinking", Kinds: []filterKind{
			k("thinking", "Thinking"),
		}},
		{Label: "System", Kinds: []filterKind{
			k("turn_duration", "Turn duration"),
			k("permission-mode", "Permission mode"),
			k("mode", "Mode"),
			k("queue-operation", "Queue op"),
			k("system", "System"),
			k("pr-link", "PR link"),
			k("frame-link", "Artifact link"),
		}},
		{Label: "Attachments", Kinds: []filterKind{
			k("attachment:file", "File attached"),
			k("attachment:compact_file_reference", "Compact file reference"),
			k("attachment:directory", "Directory listing"),
			k("attachment:plan_mode", "Plan mode"),
			k("attachment:plan_mode_exit", "Plan mode exit"),
			k("attachment:queued_command", "Queued command"),
			k("attachment:task_reminder", "Task reminder"),
			k("attachment:skill_listing", "Skill listing"),
			k("attachment:invoked_skills", "Invoked skills"),
			k("attachment:deferred_tools_delta", "Deferred tools changed"),
			k("attachment:mcp_instructions_delta", "MCP instructions changed"),
			k("attachment:agent_listing_delta", "Agent listing changed"),
			k("attachment:edited_text_file", "Edited file notice"),
			k("attachment:total_tokens_reminder", "Token reminder"),
			k("attachment:hook_success", "Hook ran"),
			k("attachment:hook_additional_context", "Hook context"),
			k("attachment:hook_system_message", "Hook message"),
			k("attachment:date_change", "Date change"),
			k("attachment:command_permissions", "Command permissions"),
			k("attachment:auto_mode", "Auto mode"),
			k("attachment:goal_status", "Goal status"),
			k("attachment:nested_memory", "Nested memory"),
			k("attachment:remote_session_change", "Remote session change"),
			k("attachment:instructions", "Instructions loaded"),
			k("attachment:prompt_snapshot", "System prompt snapshot"),
			k("attachment:deferred_tools_record", "Deferred tool schemas"),
			k("attachment:read_truncation_notice", "Read truncation notice"),
			k("attachment:auto_mode_exit", "Auto mode exit"),
			k("attachment:structured_output", "Structured output"),
			k("attachment:dynamic_skill", "Dynamic skill"),
			k("attachment:thinking_stripped", "Thinking stripped"),
			k("attachment:date", "Date"),
		}},
	}
}
