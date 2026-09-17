package pravda

import (
	"encoding/json"
	"html/template"
	"sort"
	"strings"
)

const maxInlineImageBase64 = 6 << 20

type ImageBlock struct {
	MediaType string `json:"mediaType,omitempty"`
	Data      string `json:"data,omitempty"`
}

func (i ImageBlock) DataURL() template.URL {
	if i.Data == "" || len(i.Data) > maxInlineImageBase64 {
		return ""
	}
	mt := i.MediaType
	if mt == "" {
		mt = "image/png"
	}
	return template.URL("data:" + mt + ";base64," + i.Data)
}

type contentBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

func decodeBlocks(raw json.RawMessage) []contentBlock {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var items []contentBlock
	_ = json.Unmarshal(raw, &items)
	return items
}

func decodeImages(raw json.RawMessage) []ImageBlock {
	var out []ImageBlock
	for _, b := range decodeBlocks(raw) {
		if b.Type == "image" && b.Source != nil {
			out = append(out, ImageBlock{MediaType: b.Source.MediaType, Data: b.Source.Data})
		}
	}
	return out
}

func isInterruptText(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "[Request interrupted by user") || strings.HasPrefix(s, "Turn aborted")
}

type resultBlock struct {
	Text  string
	Image *ImageBlock
}

func (e *Entry) ResultBlocks() []resultBlock {
	raw := e.ResultContent
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		return []resultBlock{{Text: decodeString(raw)}}
	}
	var out []resultBlock
	for _, b := range decodeBlocks(raw) {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, resultBlock{Text: b.Text})
			}
		case "image":
			img := ImageBlock{}
			if b.Source != nil {
				img = ImageBlock{MediaType: b.Source.MediaType, Data: b.Source.Data}
			}
			out = append(out, resultBlock{Image: &img})
		}
	}
	return out
}

type imageInfo struct {
	Image ImageBlock
	W, H  int64
	Size  int64
}

func (e *Entry) ImageResult() *imageInfo {
	var r struct {
		Type string `json:"type"`
		File struct {
			Base64       string `json:"base64"`
			Type         string `json:"type"`
			OriginalSize int64  `json:"originalSize"`
			Dimensions   struct {
				W  int64 `json:"originalWidth"`
				H  int64 `json:"originalHeight"`
				DW int64 `json:"displayWidth"`
				DH int64 `json:"displayHeight"`
			} `json:"dimensions"`
		} `json:"file"`
	}
	if len(e.ToolUseResult) > 0 && e.ToolUseResult[0] == '{' {
		if err := json.Unmarshal(e.ToolUseResult, &r); err == nil && r.Type == "image" {
			info := &imageInfo{
				Image: ImageBlock{MediaType: r.File.Type, Data: r.File.Base64},
				W:     r.File.Dimensions.W, H: r.File.Dimensions.H,
				Size: r.File.OriginalSize,
			}
			if info.W == 0 {
				info.W, info.H = r.File.Dimensions.DW, r.File.Dimensions.DH
			}
			return info
		}
	}
	for _, b := range e.ResultBlocks() {
		if b.Image != nil {
			return &imageInfo{Image: *b.Image}
		}
	}
	return nil
}

type askOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type askQuestion struct {
	Header      string      `json:"header"`
	Question    string      `json:"question"`
	MultiSelect bool        `json:"multiSelect"`
	Options     []askOption `json:"options"`
	Answer      string
}

func (q askQuestion) Chosen(label string) bool {
	if q.Answer == "" {
		return false
	}
	if q.Answer == label {
		return true
	}
	for _, part := range strings.Split(q.Answer, ",") {
		if strings.TrimSpace(part) == label {
			return true
		}
	}
	return false
}

func (e *Entry) Questions() []askQuestion {
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	_ = json.Unmarshal(e.Input, &in)
	var res struct {
		Answers map[string]string `json:"answers"`
	}
	if len(e.ToolUseResult) > 0 && e.ToolUseResult[0] == '{' {
		_ = json.Unmarshal(e.ToolUseResult, &res)
	}
	for i := range in.Questions {
		in.Questions[i].Answer = res.Answers[in.Questions[i].Question]
	}
	return in.Questions
}

func (e *Entry) InputKeys() []string {
	keys := make([]string, 0, len(e.InputMap))
	for k := range e.InputMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (e *Entry) InputStrings(key string) []string {
	var out []string
	for _, raw := range rawList(e.Input, key) {
		out = append(out, decodeString(raw))
	}
	return out
}

func (e *Entry) ResultField(key string) string {
	if len(e.ToolUseResult) == 0 || e.ToolUseResult[0] != '{' {
		return ""
	}
	return rawField(e.ToolUseResult, key)
}

func (e *Entry) ResultPath(keys ...string) string {
	if len(e.ToolUseResult) == 0 || e.ToolUseResult[0] != '{' {
		return ""
	}
	return rawPath(e.ToolUseResult, keys...)
}

func (e *Entry) ResultList(key string) []json.RawMessage {
	if len(e.ToolUseResult) == 0 || e.ToolUseResult[0] != '{' {
		return nil
	}
	return rawList(e.ToolUseResult, key)
}

func (e *Entry) ResultTextPlain() string {
	var parts []string
	for _, b := range e.ResultBlocks() {
		if b.Text != "" {
			parts = append(parts, b.Text)
		} else if b.Image != nil {
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

func (e *Entry) BriefTitle() string {
	f := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(field(e, k)); v != "" {
				return v
			}
		}
		return ""
	}
	switch e.Name {
	case "Monitor":
		return f("description", "command")
	case "TaskStop", "TaskOutput":
		if id := f("task_id", "taskId"); id != "" {
			return "task " + id
		}
	case "SendMessage":
		to := f("to", "recipient")
		body := firstLine(f("summary", "message", "content"))
		switch {
		case to != "" && body != "":
			return "→ " + to + ": " + body
		case to != "":
			return "→ " + to
		default:
			return body
		}
	case "ListAgents":
		return "list agents"
	case "PushNotification":
		return f("message", "title")
	case "EnterPlanMode":
		return "enter plan mode"
	case "ExitPlanMode":
		return "exit plan mode"
	}
	return toolPreview(e)
}

type instrFile struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (e *Entry) InstructionFiles() []instrFile {
	var a struct {
		Files []instrFile `json:"files"`
	}
	_ = json.Unmarshal(e.Attach, &a)
	return a.Files
}

type promptSnapshot struct {
	Sections int
	Bytes    int64
	Tools    int
	Parts    []string
}

func (e *Entry) PromptSnapshot() promptSnapshot {
	var a struct {
		SystemPrompt []string          `json:"systemPrompt"`
		Tools        []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(e.Attach, &a)
	ps := promptSnapshot{Sections: len(a.SystemPrompt), Tools: len(a.Tools), Parts: a.SystemPrompt}
	for _, p := range a.SystemPrompt {
		ps.Bytes += int64(len(p))
	}
	return ps
}

func (e *Entry) DeferredToolNames() []string {
	var out []string
	for _, raw := range rawList(e.Attach, "entries") {
		if n := rawField(raw, "name"); n != "" {
			out = append(out, n)
		}
	}
	return out
}

type structuredOutput struct {
	HasPassed bool
	Passed    bool
	Problems  []string
	Summary   string
	Keys      []string
}

func (e *Entry) StructuredOutput() structuredOutput {
	var a struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(e.Attach, &a)
	so := structuredOutput{}
	if raw, ok := a.Data["passed"]; ok {
		so.HasPassed = json.Unmarshal(raw, &so.Passed) == nil
	}
	if raw, ok := a.Data["problems"]; ok {
		var list []json.RawMessage
		_ = json.Unmarshal(raw, &list)
		for _, p := range list {
			so.Problems = append(so.Problems, decodeString(p))
		}
	}
	for _, k := range []string{"summary", "evidence", "feature", "reason"} {
		if raw, ok := a.Data[k]; ok {
			if s := decodeString(raw); s != "" && raw[0] == '"' {
				so.Summary = s
				break
			}
		}
	}
	for k := range a.Data {
		so.Keys = append(so.Keys, k)
	}
	sort.Strings(so.Keys)
	return so
}

func (e *Entry) AttachStrings(key string) []string {
	var out []string
	for _, raw := range rawList(e.Attach, key) {
		out = append(out, decodeString(raw))
	}
	return out
}

func (e *Entry) AttachBool(key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.Attach, &m); err != nil {
		return false
	}
	var b bool
	_ = json.Unmarshal(m[key], &b)
	return b
}
