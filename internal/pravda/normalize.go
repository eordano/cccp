package pravda

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
)

func ParseJSONL(r io.Reader) ([]*RawEvent, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	var out []*RawEvent
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev RawEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		raw := make([]byte, len(line))
		copy(raw, line)
		ev.Raw = raw
		out = append(out, &ev)
	}
	return out, sc.Err()
}

type Session struct {
	Entries  []Entry
	Metadata string
	Events   []*RawEvent
	Facts    *sessionFacts
}

type SubAgentLoader func(agentID string) *Session

func Normalize(events []*RawEvent, loadSubAgent SubAgentLoader) *Session {
	enrichQueueOps(events)

	byUUID := make(map[string]*RawEvent, len(events))
	toolResults := make(map[string]toolResult)
	for _, e := range events {
		if e.UUID != "" {
			byUUID[e.UUID] = e
		}
		if e.Type == "user" && e.Message != nil {
			for _, item := range decodeContent(e.Message.Content) {
				if item.Type == "tool_result" && item.ToolUseID != "" {
					toolResults[item.ToolUseID] = toolResult{event: e, item: item}
				}
			}
		}
	}

	sidechainByTaskID := groupSidechains(events, byUUID)
	claimed := make(map[*RawEvent]bool)
	for _, list := range sidechainByTaskID {
		for _, ev := range list {
			claimed[ev] = true
		}
	}

	ctx := &normalizeCtx{
		toolResults:       toolResults,
		sidechainByTaskID: sidechainByTaskID,
		loadSubAgent:      loadSubAgent,
		seenPRURLs:        make(map[string]bool),
	}

	var entries []Entry
	for _, e := range events {
		if claimed[e] {
			continue
		}
		entries = append(entries, entriesFor(e, ctx)...)
	}

	for i := range entries {
		entries[i].Index = i + 1
	}
	entries = dropCodexItemDuplicates(entries)
	return &Session{
		Entries:  entries,
		Metadata: buildMetadata(entries),
		Events:   events,
	}
}

type toolResult struct {
	event *RawEvent
	item  *ContentItem
}

type normalizeCtx struct {
	toolResults       map[string]toolResult
	sidechainByTaskID map[string][]*RawEvent
	loadSubAgent      SubAgentLoader

	lastPermMode string
	lastMode     string
	lastModel    string
	lastEffort   string
	lastDate     string
	seenPRURLs   map[string]bool
}

func enrichQueueOps(events []*RawEvent) {
	var pending []QueuedItem
	for _, e := range events {
		if e.Type != "queue-operation" {
			continue
		}
		ctx := applyQueueOp(&pending, e)
		e.queueCtx = ctx
	}
}

func applyQueueOp(pending *[]QueuedItem, raw *RawEvent) *QueueCtx {
	content := decodeString(raw.Content)
	switch raw.Operation {
	case "enqueue":
		*pending = append(*pending, QueuedItem{Content: content, TS: raw.Timestamp})
		return &QueueCtx{Action: "enqueue", Item: content, Depth: len(*pending), Position: len(*pending)}
	case "dequeue", "remove":
		var popped QueuedItem
		if len(*pending) > 0 {
			popped = (*pending)[0]
			*pending = (*pending)[1:]
		}
		return &QueueCtx{Action: raw.Operation, Item: popped.Content, QueuedAt: popped.TS, Depth: len(*pending)}
	case "popAll":
		items := make([]QueuedItem, len(*pending))
		copy(items, *pending)
		*pending = nil
		return &QueueCtx{Action: "popAll", Items: items, Depth: 0}
	}
	return nil
}

func groupSidechains(events []*RawEvent, byUUID map[string]*RawEvent) map[string][]*RawEvent {
	out := make(map[string][]*RawEvent)
	for _, e := range events {
		if !e.IsSidechain {
			continue
		}
		cur := e
		parent := byUUID[cur.ParentUUID]
		for parent != nil && parent.IsSidechain {
			cur = parent
			parent = byUUID[cur.ParentUUID]
		}
		if parent == nil || parent.Type != "assistant" || parent.Message == nil {
			continue
		}
		for _, item := range decodeContent(parent.Message.Content) {
			if item.Type == "tool_use" && (item.Name == "Agent" || item.Name == "Task") {
				out[item.ID] = append(out[item.ID], e)
				break
			}
		}
	}
	return out
}

func decodeContent(raw json.RawMessage) []*ContentItem {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		return nil
	}
	var items []*ContentItem
	_ = json.Unmarshal(raw, &items)
	return items
}

func decodeString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func entriesFor(e *RawEvent, ctx *normalizeCtx) []Entry {
	ts := e.Timestamp
	var out []Entry
	switch e.Type {
	case "user":
		out = userEntries(e, ts)
	case "assistant":
		out = assistantEntries(e, ts, ctx)
	case "system":
		out = systemEntries(e, ts)
	case "attachment":
		out = attachmentEntries(e, ts, ctx)
	case "model-change":
		out = modelChangeEntries(e.Model, e.Effort, ts, ctx)
	case "permission-mode":
		if e.PermissionMode == ctx.lastPermMode {
			return nil
		}
		ctx.lastPermMode = e.PermissionMode
		out = []Entry{{Kind: KindPermissionMode, TS: ts, Mode: e.PermissionMode}}
	case "mode":
		if e.Mode == ctx.lastMode {
			return nil
		}
		ctx.lastMode = e.Mode
		out = []Entry{{Kind: KindMode, TS: ts, Mode: e.Mode}}
	case "pr-link":
		if e.PRUrl == "" || ctx.seenPRURLs[e.PRUrl] {
			return nil
		}
		ctx.seenPRURLs[e.PRUrl] = true
		out = []Entry{{Kind: KindPRLink, TS: ts, URL: e.PRUrl, Repo: e.PRRepository, Number: e.PRNumber}}
	case "frame-link":
		out = []Entry{{Kind: KindFrameLink, TS: ts, URL: e.FrameURL, Text: e.Title, Path: e.Path}}
	case "queue-operation":
		qc := e.queueCtx
		entry := Entry{Kind: KindQueueOperation, TS: ts, Op: e.Operation}
		if qc != nil {
			entry.Item = qc.Item
			entry.QueuedAt = qc.QueuedAt
			entry.Items = qc.Items
			entry.Depth = qc.Depth
			entry.Position = qc.Position
		}
		entry.Text = decodeString(e.Content)
		out = []Entry{entry}
	case "started", "result":
		if e.AgentID == "" {
			out = []Entry{{Kind: KindUnknown, TS: ts, Data: e.Raw}}
			break
		}
		entry := Entry{Kind: KindWorkflow, TS: ts, AgentID: e.AgentID, Op: e.Type}
		if e.Type == "result" {
			entry.Summary = gjsonString(e.Result, "summary")
			if entry.Summary == "" {
				entry.Summary = decodeString(e.Result)
			}
			entry.HasResult = true
		}
		out = []Entry{entry}
	case "file-history-snapshot", "file-history-delta", "last-prompt",
		"bridge-session", "ai-title", "custom-title", "agent-name", "summary",
		"atis-latch", "cost-state", "session-meta", "usage", "fork-context-ref":
		return nil
	default:
		out = []Entry{{Kind: KindUnknown, TS: ts, Data: e.Raw}}
	}
	for i := range out {
		out[i].RawSource = e
	}
	return out
}

var (
	cmdNameRe      = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	cmdMessageRe   = regexp.MustCompile(`(?s)<command-message>(.*?)</command-message>`)
	cmdArgsRe      = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	cmdStdoutRe    = regexp.MustCompile(`(?s)<local-command-stdout>([\s\S]*)</local-command-stdout>`)
	taskNotifRe    = regexp.MustCompile(`^<task-notification>`)
	taskIDRe       = regexp.MustCompile(`(?s)<task-id>(.*?)</task-id>`)
	taskTUIDRe     = regexp.MustCompile(`(?s)<tool-use-id>(.*?)</tool-use-id>`)
	taskStatusRe   = regexp.MustCompile(`(?s)<status>(.*?)</status>`)
	taskSummaryRe  = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	taskResultRe   = regexp.MustCompile(`(?s)<result>(.*?)</result>`)
	taskOutputFile = regexp.MustCompile(`(?s)<output-file>(.*?)</output-file>`)
	bashInputRe    = regexp.MustCompile(`(?s)<bash-input>(.*?)</bash-input>`)
	bashStdoutRe   = regexp.MustCompile(`(?s)<bash-stdout>(.*?)</bash-stdout>`)
	bashStderrRe   = regexp.MustCompile(`(?s)<bash-stderr>(.*?)</bash-stderr>`)
	totalTokensRe  = regexp.MustCompile(`(\d+)\s*tokens left`)
)

func attachmentEntries(e *RawEvent, ts string, ctx *normalizeCtx) []Entry {
	subtype := gjsonString(e.Attachment, "type")
	entry := Entry{Kind: KindAttachment, TS: ts, Subtype: subtype, Attach: e.Attachment}
	switch subtype {
	case "session_context", "silent_turn_reminder", "environment",
		"batching_reminder_sent", "bash_output_audience_note":
		return nil
	case "model":
		name := rawPath(e.Attachment, "identity", "marketingName")
		if name == "" {
			name = rawPath(e.Attachment, "identity", "modelId")
		}
		return modelChangeEntries(name, "", ts, ctx)
	case "date":
		d := gjsonString(e.Attachment, "date")
		prev := ctx.lastDate
		if d != "" {
			ctx.lastDate = d
		}
		if d == "" || prev == "" || d == prev {
			return nil
		}
		entry.Subtype = "date_change"
		entry.Text = d
	case "date_change":
		entry.Text = gjsonString(e.Attachment, "newDate")
		if entry.Text != "" {
			ctx.lastDate = entry.Text
		}
	case "total_tokens_reminder":
		if m := totalTokensRe.FindStringSubmatch(gjsonString(e.Attachment, "text")); m != nil {
			entry.TokensLeft, _ = strconv.ParseInt(m[1], 10, 64)
		}
	case "read_truncation_notice":
		entry.Text = gjsonString(e.Attachment, "banner")
		entry.ToolUseID = gjsonString(e.Attachment, "toolUseID")
	case "remote_session_change", "bridge_status":
		entry.URL = gjsonString(e.Attachment, "url")
	}
	return []Entry{entry}
}

func modelChangeEntries(model, effort, ts string, ctx *normalizeCtx) []Entry {
	if model == "" || (model == ctx.lastModel && effort == ctx.lastEffort) {
		return nil
	}
	ctx.lastModel, ctx.lastEffort = model, effort
	text := "model → " + model
	if effort != "" {
		text += " (effort " + effort + ")"
	}
	return []Entry{{Kind: KindMode, TS: ts, Subtype: "model-change", Mode: model, Effort: effort, Text: text}}
}

func systemEntries(e *RawEvent, ts string) []Entry {
	switch e.Subtype {
	case "turn_duration":
		entry := Entry{Kind: KindTurnDuration, TS: ts, DurationMs: e.DurationMs, MessageCount: e.MessageCount}
		if errText := gjsonString(e.Raw, "error"); errText != "" {
			entry.Text = errText
			entry.Level = "error"
		}
		return []Entry{entry}
	case "goal":
		entry := Entry{Kind: KindSystem, TS: ts, Subtype: e.Subtype, Text: decodeString(e.Content), Level: e.Level, Data: e.Raw}
		for _, k := range []string{"goal", "text", "summary"} {
			if entry.Text != "" {
				break
			}
			entry.Text = gjsonString(e.Raw, k)
		}
		entry.Status = gjsonString(e.Raw, "status")
		return []Entry{entry}
	case "local_command":
		s := decodeString(e.Content)
		return []Entry{{
			Kind: KindCommand, TS: ts,
			CommandName:    firstMatch(cmdNameRe, s),
			CommandMessage: firstMatch(cmdMessageRe, s),
			CommandArgs:    firstMatch(cmdArgsRe, s),
		}}
	case "model_refusal_no_fallback", "model_refusal_fallback":
		var fb struct {
			FallbackModel string   `json:"fallbackModel"`
			Direction     string   `json:"direction"`
			Retracted     []string `json:"retractedMessageUuids"`
		}
		_ = json.Unmarshal(e.Raw, &fb)
		text := e.APIRefusalExplanation
		if text == "" {
			text = decodeString(e.Content)
		}
		return []Entry{{
			Kind: KindSystem, TS: ts, Subtype: e.Subtype,
			Status:    e.APIRefusalCategory,
			Text:      text,
			Mode:      e.OriginalModel,
			ToModel:   fb.FallbackModel,
			Direction: fb.Direction,
			Retracted: len(fb.Retracted),
			Level:     e.Level,
			Data:      e.Raw,
		}}
	case "bridge_status":
		return []Entry{{Kind: KindSystem, TS: ts, Subtype: e.Subtype, Text: decodeString(e.Content), URL: gjsonString(e.Raw, "url"), Data: e.Raw}}
	case "informational":
		return []Entry{{Kind: KindSystem, TS: ts, Subtype: e.Subtype, Text: decodeString(e.Content), Level: e.Level, Data: e.Raw}}
	case "compact_boundary":
		entry := Entry{Kind: KindSystem, TS: ts, Subtype: e.Subtype, Text: decodeString(e.Content), Data: e.Raw}
		var cm struct {
			Trigger    string `json:"trigger"`
			PreTokens  int64  `json:"preTokens"`
			PostTokens int64  `json:"postTokens"`
			DurationMs int64  `json:"durationMs"`
		}
		if err := json.Unmarshal(e.CompactMetadata, &cm); err == nil {
			entry.Trigger = cm.Trigger
			entry.PreTokens = cm.PreTokens
			entry.PostTokens = cm.PostTokens
			entry.DurationMs = cm.DurationMs
		}
		return []Entry{entry}
	default:
		return []Entry{{Kind: KindSystem, TS: ts, Subtype: e.Subtype, Text: decodeString(e.Content), HookCount: e.HookCount, Data: e.Raw}}
	}
}

func userEntries(e *RawEvent, ts string) []Entry {
	if e.Message == nil {
		return nil
	}
	content := e.Message.Content
	if len(content) > 0 && content[0] == '[' {
		return userListEntries(e, ts)
	}
	s := decodeString(content)
	if s == "" {
		return nil
	}
	if taskNotifRe.MatchString(s) {
		return []Entry{parseTaskNotification(s, ts)}
	}
	if strings.HasPrefix(s, "<bash-input>") {
		return []Entry{{Kind: KindBashInput, TS: ts, Text: firstMatch(bashInputRe, s)}}
	}
	if strings.HasPrefix(s, "<bash-stdout>") || strings.HasPrefix(s, "<bash-stderr>") {
		return []Entry{{
			Kind: KindBashOutput, TS: ts,
			Text:   strings.TrimRight(firstMatch(bashStdoutRe, s), "\n"),
			Stderr: strings.TrimRight(firstMatch(bashStderrRe, s), "\n"),
		}}
	}
	if isInterruptText(s) {
		return []Entry{{Kind: KindInterrupt, TS: ts, Text: s}}
	}
	if cmdNameRe.MatchString(s) || cmdMessageRe.MatchString(s) {
		return []Entry{{
			Kind: KindCommand, TS: ts,
			CommandName:    firstMatch(cmdNameRe, s),
			CommandMessage: firstMatch(cmdMessageRe, s),
			CommandArgs:    firstMatch(cmdArgsRe, s),
		}}
	}
	if strings.HasPrefix(s, "<local-command-stdout>") {
		body := firstMatch(cmdStdoutRe, s)
		if body == "" {
			body = s
		}
		return []Entry{{Kind: KindCommandOutput, TS: ts, Text: body}}
	}
	if e.IsMeta {
		return []Entry{{Kind: KindMeta, TS: ts, Text: s}}
	}
	return []Entry{{Kind: KindUser, TS: ts, Text: s, CWD: e.CWD}}
}

func userListEntries(e *RawEvent, ts string) []Entry {
	var out []Entry
	var texts []string
	meta := e.IsMeta
	for _, it := range decodeContent(e.Message.Content) {
		if it.Type != "text" || strings.TrimSpace(it.Text) == "" {
			continue
		}
		if isInterruptText(it.Text) {
			out = append(out, Entry{Kind: KindInterrupt, TS: ts, Text: strings.TrimSpace(it.Text)})
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(it.Text), "<fork-boilerplate>") {
			meta = true
		}
		texts = append(texts, it.Text)
	}
	images := decodeImages(e.Message.Content)
	if len(texts) == 0 && len(images) == 0 {
		return out
	}
	entry := Entry{Kind: KindUser, TS: ts, Text: strings.Join(texts, "\n\n"), CWD: e.CWD, Images: images}
	if meta {
		entry.Kind = KindMeta
	}
	return append(out, entry)
}

func parseTaskNotification(content, ts string) Entry {
	return Entry{
		Kind: KindAgentResult, TS: ts,
		AgentID:    firstMatch(taskIDRe, content),
		ToolUseID:  firstMatch(taskTUIDRe, content),
		Status:     firstMatch(taskStatusRe, content),
		Summary:    firstMatch(taskSummaryRe, content),
		Result:     firstMatch(taskResultRe, content),
		OutputFile: firstMatch(taskOutputFile, content),
	}
}

func assistantEntries(e *RawEvent, ts string, ctx *normalizeCtx) []Entry {
	if e.Message == nil {
		return nil
	}
	items := decodeContent(e.Message.Content)
	var rawItems []json.RawMessage
	var out []Entry
	for i, item := range items {
		switch item.Type {
		case "text":
			if strings.TrimSpace(item.Text) != "" {
				out = append(out, Entry{Kind: KindMessage, TS: ts, Text: item.Text, Model: e.Message.Model})
			}
		case "thinking":
			out = append(out, Entry{Kind: KindThinking, TS: ts, Text: item.Thinking, HasBody: item.Thinking != ""})
		case "fallback":
			if rawItems == nil {
				_ = json.Unmarshal(e.Message.Content, &rawItems)
			}
			var fb struct {
				From struct {
					Model string `json:"model"`
				} `json:"from"`
				To struct {
					Model string `json:"model"`
				} `json:"to"`
			}
			if i < len(rawItems) {
				_ = json.Unmarshal(rawItems[i], &fb)
			}
			out = append(out, Entry{
				Kind: KindSystem, TS: ts, Subtype: "model_fallback",
				Mode: fb.From.Model, ToModel: fb.To.Model,
				Text: "model fallback " + fb.From.Model + " → " + fb.To.Model,
			})
		case "tool_use":
			entry := Entry{
				Kind: KindTool, TS: ts,
				Name:       item.Name,
				ID:         item.ID,
				Input:      item.Input,
				RawToolUse: item,
			}
			if len(item.Input) > 0 {
				_ = json.Unmarshal(item.Input, &entry.InputMap)
			}
			if r, ok := ctx.toolResults[item.ID]; ok {
				entry.ResultContent = r.item.RContent
				entry.IsError = r.item.IsError
				entry.ToolUseResult = r.event.ToolUseResult
				entry.HasResult = true
				entry.RawResult = r.event
			}
			if item.Name == "Agent" || item.Name == "Task" {
				if len(entry.ToolUseResult) > 0 {
					var ref AgentRef
					if err := json.Unmarshal(entry.ToolUseResult, &ref); err == nil && ref.AgentID != "" {
						entry.AgentRef = &ref
					}
				}
				switch {
				case len(ctx.sidechainByTaskID[item.ID]) > 0:
					for _, sub := range ctx.sidechainByTaskID[item.ID] {
						entry.SubAgent = append(entry.SubAgent, entriesFor(sub, ctx)...)
					}
				case entry.AgentRef != nil && ctx.loadSubAgent != nil:
					if sub := ctx.loadSubAgent(entry.AgentRef.AgentID); sub != nil {
						entry.SubAgent = sub.Entries
					}
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

func firstMatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func gjsonString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v, ok := m[key]; ok {
		return decodeString(v)
	}
	return ""
}

func buildMetadata(entries []Entry) string {
	if len(entries) == 0 {
		return "0 events"
	}
	var firstTS, lastTS string
	toolCount := 0
	for _, e := range entries {
		if firstTS == "" && e.TS != "" {
			firstTS = e.TS
		}
		if e.TS != "" {
			lastTS = e.TS
		}
		if e.Kind == KindTool {
			toolCount++
		}
	}
	var pieces []string
	if firstTS != "" {
		if t, ok := parseTS(firstTS); ok {
			pieces = append(pieces, t.Format("2006-01-02 15:04"))
		}
	}
	pieces = append(pieces, itoa(len(entries))+" events")
	if toolCount > 0 {
		pieces = append(pieces, itoa(toolCount)+" tools")
	}
	if firstTS != "" && lastTS != "" {
		a, aok := parseTS(firstTS)
		b, bok := parseTS(lastTS)
		if aok && bok && b.After(a) {
			pieces = append(pieces, formatDuration(b.Sub(a)))
		}
	}
	return strings.Join(pieces, " · ")
}

func dropCodexItemDuplicates(entries []Entry) []Entry {
	key := func(e *Entry) string {
		for _, k := range []string{"file_path", "query", "url"} {
			if v, ok := e.InputMap[k]; ok {
				return k + "=" + string(v)
			}
		}
		return ""
	}
	out := entries[:0]
	lastTool := -1
	for i := range entries {
		e := &entries[i]
		if e.Kind == KindTool {
			if e.RawSource != nil && e.RawSource.CodexItem != "" && lastTool >= 0 {
				if k := key(e); k != "" && k == key(&out[lastTool]) {
					continue
				}
			}
			out = append(out, *e)
			lastTool = len(out) - 1
			continue
		}
		out = append(out, *e)
	}
	return out
}
