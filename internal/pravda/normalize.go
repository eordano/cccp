package pravda

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
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
		if e.Subtype == "turn_duration" {
			out = []Entry{{Kind: KindTurnDuration, TS: ts, DurationMs: e.DurationMs, MessageCount: e.MessageCount}}
		} else {
			out = []Entry{{Kind: KindSystem, TS: ts, Data: e.Raw}}
		}
	case "attachment":
		subtype := gjsonString(e.Attachment, "type")
		out = []Entry{{Kind: KindAttachment, TS: ts, Subtype: subtype, Attach: e.Attachment}}
	case "permission-mode":
		out = []Entry{{Kind: KindPermissionMode, TS: ts, Mode: e.PermissionMode}}
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
	case "file-history-snapshot", "last-prompt":
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
)

func userEntries(e *RawEvent, ts string) []Entry {
	if e.Message == nil {
		return nil
	}
	content := e.Message.Content
	if len(content) > 0 && content[0] == '[' {
		items := decodeContent(content)
		allResults := len(items) > 0
		for _, it := range items {
			if it.Type != "tool_result" {
				allResults = false
				break
			}
		}
		if allResults {
			return nil
		}
		var out []Entry
		for _, it := range items {
			if it.Type == "text" && it.Text != "" {
				out = append(out, Entry{Kind: KindInterrupt, TS: ts, Text: it.Text})
			}
		}
		return out
	}
	s := decodeString(content)
	if s == "" {
		return nil
	}
	if taskNotifRe.MatchString(s) {
		return []Entry{parseTaskNotification(s, ts)}
	}
	if strings.HasPrefix(s, "<command-name>") {
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
	var out []Entry
	for _, item := range items {
		switch item.Type {
		case "text":
			if strings.TrimSpace(item.Text) != "" {
				out = append(out, Entry{Kind: KindMessage, TS: ts, Text: item.Text})
			}
		case "thinking":
			out = append(out, Entry{Kind: KindThinking, TS: ts, Text: item.Thinking, HasBody: item.Thinking != ""})
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
