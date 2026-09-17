package pravda

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func init() {
	RegisterConverter("codex", convertCodex)
	RegisterSessionFilter("codex", func(rel string) bool {
		return strings.HasPrefix(rel, "sessions/") || strings.HasPrefix(rel, "archived_sessions/")
	})
}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   []codexContent  `json:"content"`
	Summary   []codexContent  `json:"summary"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Message   string          `json:"message"`
	Reason    string          `json:"reason"`
	Duration  int64           `json:"duration_ms"`
	TTFT      int64           `json:"time_to_first_token_ms"`
	Author    string          `json:"author"`
	Recipient string          `json:"recipient"`
	Error     *struct {
		Message string `json:"message"`
		Code    string `json:"codex_error_info"`
	} `json:"error"`
	Item           json.RawMessage `json:"item"`
	Info           json.RawMessage `json:"info"`
	ThreadSettings json.RawMessage `json:"thread_settings"`
	Goal           json.RawMessage `json:"goal"`

	WindowNumber       int               `json:"window_number"`
	ReplacementHistory []json.RawMessage `json:"replacement_history"`

	CWD            string `json:"cwd"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	ParentThreadID string `json:"parent_thread_id"`
	CLIVersion     string `json:"cli_version"`
	ThreadSource   string `json:"thread_source"`
	AgentNickname  string `json:"agent_nickname"`
	AgentPath      string `json:"agent_path"`
	ModelProvider  string `json:"model_provider"`
	Collab         struct {
		Settings struct {
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"settings"`
	} `json:"collaboration_mode"`
	BaseInstructions struct {
		Provenance struct {
			Model string `json:"model"`
		} `json:"provenance"`
	} `json:"base_instructions"`
	Git struct {
		Branch     string `json:"branch"`
		Commit     string `json:"commit_hash"`
		Repository string `json:"repository_url"`
	} `json:"git"`
}

type codexContent struct {
	Type             string `json:"type"`
	Text             string `json:"text"`
	EncryptedContent string `json:"encrypted_content"`
}

type codexUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
}

var (
	codexExitRe      = regexp.MustCompile(`(?m)^Process exited with code (\d+)`)
	codexToolRe      = regexp.MustCompile(`tools\.([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	codexHeaderRe    = regexp.MustCompile(`^Script (completed|failed|terminated|running with cell ID \d+)\nWall time [\d.]+ seconds\nOutput:\n?$`)
	codexEncryptedRe = regexp.MustCompile(`^gAAAAA[A-Za-z0-9_=-]{20,}$`)
	codexAgentHdrRe  = regexp.MustCompile(`^Message Type: [A-Z_]+\nTask name: [^\n]*\nSender: [^\n]*\nPayload:\n?`)
	codexPatchFileRe = regexp.MustCompile(`(?m)^\*\*\* (Add|Update|Delete) File: (.+)$`)
	codexMovePathRe  = regexp.MustCompile(`(?m)^\*\*\* Move to: (.+)$`)
)

var codexDroppedDeveloper = []string{
	"<permissions instructions>", "<skills_instructions>", "<apps_instructions>",
	"Use prior reviews as context",
}

func convertCodex(line []byte) [][]byte {
	var l codexLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}
	var p codexPayload
	if len(l.Payload) > 0 {
		_ = json.Unmarshal(l.Payload, &p)
	}
	uid := p.ID
	if uid == "" && p.CallID != "" {
		uid = p.Type + "-" + p.CallID
	}
	if uid == "" {
		sum := sha1.Sum(line)
		uid = "codex-" + hex.EncodeToString(sum[:8])
	}
	switch l.Type {
	case "session_meta":
		return one(codexSessionMeta(&p, uid, l.Timestamp))
	case "turn_context":
		effort := p.Effort
		if effort == "" {
			effort = p.Collab.Settings.ReasoningEffort
		}
		return codexModelChange(p.Model, effort, uid, l.Timestamp)
	case "response_item":
		return codexResponseItem(&p, uid, l.Timestamp)
	case "event_msg":
		return codexEventMsg(&p, uid, l.Timestamp)
	case "compacted":
		text := "Context compacted"
		if p.WindowNumber > 0 {
			text = fmt.Sprintf("Context compacted (window %d, %d messages kept)", p.WindowNumber, len(p.ReplacementHistory))
		}
		return one(codexEvent("system", uid, l.Timestamp, map[string]any{
			"subtype": "compact_boundary",
			"content": text,
			"compactMetadata": map[string]any{
				"trigger":            "auto",
				"windowNumber":       p.WindowNumber,
				"replacementHistory": len(p.ReplacementHistory),
			},
		}))
	}
	return nil
}

func codexSessionMeta(p *codexPayload, uid, ts string) []byte {
	model := p.Model
	if model == "" {
		model = p.BaseInstructions.Provenance.Model
	}
	fields := map[string]any{
		"cwd":             p.CWD,
		"sessionId":       p.ID,
		"parentSessionId": p.ParentThreadID,
		"agentSource":     p.ThreadSource,
		"version":         p.CLIVersion,
		"gitBranch":       p.Git.Branch,
		"gitCommit":       p.Git.Commit,
		"gitRepo":         p.Git.Repository,
		"model":           model,
		"modelProvider":   p.ModelProvider,
		"agentNickname":   p.AgentNickname,
		"agentPath":       p.AgentPath,
	}
	if model != "" {
		fields["message"] = map[string]any{"role": "assistant", "model": model, "content": []any{}}
	}
	return codexEvent("session-meta", uid, ts, fields)
}

func codexModelChange(model, effort, uid, ts string) [][]byte {
	if model == "" {
		return nil
	}
	return one(codexEvent("model-change", uid, ts, map[string]any{"model": model, "effort": effort}))
}

func codexEventMsg(p *codexPayload, uid, ts string) [][]byte {
	switch p.Type {
	case "task_complete":
		fields := map[string]any{"subtype": "turn_duration", "durationMs": p.Duration}
		if p.TTFT > 0 {
			fields["timeToFirstTokenMs"] = p.TTFT
		}
		if p.Error != nil && (p.Error.Code != "" || p.Error.Message != "") {
			code := p.Error.Code
			if code == "" {
				code = "error"
			}
			fields["error"] = code
			fields["errorMessage"] = p.Error.Message
		}
		return one(codexEvent("system", uid, ts, fields))
	case "turn_aborted":
		text := "Turn aborted"
		if p.Reason != "" {
			text += ": " + p.Reason
		}
		return one(codexMessage("user", uid, ts, "user",
			[]map[string]any{{"type": "text", "text": text}}, nil))
	case "token_count":
		var info struct {
			Last *codexUsage `json:"last_token_usage"`
		}
		if json.Unmarshal(p.Info, &info) != nil || info.Last == nil {
			return nil
		}
		u := info.Last
		input := u.Input - u.Cached
		if input < 0 {
			input = u.Input
		}
		return one(codexEvent("usage", uid, ts, map[string]any{
			"model": "",
			"usage": map[string]any{
				"input_tokens":                input,
				"output_tokens":               u.Output,
				"cache_read_input_tokens":     u.Cached,
				"cache_creation_input_tokens": u.CacheWrite,
			},
			"costUSD": 0,
		}))
	case "thread_settings_applied":
		var ts2 struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		_ = json.Unmarshal(p.ThreadSettings, &ts2)
		return codexModelChange(ts2.Model, ts2.Effort, uid, ts)
	case "thread_goal_updated":
		var g struct {
			Objective string `json:"objective"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(p.Goal, &g) != nil || g.Objective == "" {
			return nil
		}
		return one(codexEvent("system", uid, ts, map[string]any{
			"subtype": "goal", "content": g.Objective, "status": g.Status, "goal": json.RawMessage(p.Goal),
		}))
	case "item_completed":
		return codexItemCompleted(p.Item, uid, ts)
	}
	return nil
}

type codexItem struct {
	Type          string                     `json:"type"`
	ID            string                     `json:"id"`
	Kind          string                     `json:"kind"`
	Path          string                     `json:"path"`
	Status        string                     `json:"status"`
	Stdout        string                     `json:"stdout"`
	Stderr        string                     `json:"stderr"`
	Changes       map[string]codexFileChange `json:"changes"`
	AgentThreadID string                     `json:"agent_thread_id"`
	AgentPath     string                     `json:"agent_path"`
	Query         string                     `json:"query"`
	Action        struct {
		Type    string   `json:"type"`
		URL     string   `json:"url"`
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
		Pattern string   `json:"pattern"`
	} `json:"action"`
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	} `json:"results"`
	UserFacingHint string `json:"user_facing_hint"`
	ReviewOutput   *struct {
		Correctness string `json:"overall_correctness"`
		Explanation string `json:"overall_explanation"`
	} `json:"review_output"`
}

type codexFileChange struct {
	Type        string `json:"type"`
	Content     string `json:"content"`
	UnifiedDiff string `json:"unified_diff"`
	MovePath    string `json:"move_path"`
}

func codexItemCompleted(raw json.RawMessage, uid, ts string) [][]byte {
	var it codexItem
	if json.Unmarshal(raw, &it) != nil || it.ID == "" {
		return nil
	}
	marker := map[string]any{"codexItem": it.Type}
	switch it.Type {
	case "FileChange":
		paths := make([]string, 0, len(it.Changes))
		for k := range it.Changes {
			paths = append(paths, k)
		}
		sort.Strings(paths)
		var out [][]byte
		var results []map[string]any
		for i, fp := range paths {
			ch := it.Changes[fp]
			id := it.ID
			if len(paths) > 1 {
				id = fmt.Sprintf("%s#%d", it.ID, i)
			}
			name, input := codexChangeInput(fp, ch)
			out = append(out, codexToolUse(uid+"-"+strconv.Itoa(i), ts, id, name, input, marker))
			results = append(results, codexResultFromStatus(id, it.Status, it.Stdout, it.Stderr))
		}
		if len(results) > 0 {
			out = append(out, codexMessage("user", uid+"-r", ts, "user", results, marker))
		}
		return out
	case "ImageView":
		fp := strings.TrimPrefix(it.Path, "file://")
		return [][]byte{
			codexToolUse(uid, ts, it.ID, "Read", map[string]any{"file_path": fp}, marker),
			codexMessage("user", uid+"-r", ts, "user", []map[string]any{{
				"type": "tool_result", "tool_use_id": it.ID, "content": "[image]",
			}}, marker),
		}
	case "Extension":
		name, input := "WebSearch", map[string]any{"query": it.Query}
		switch it.Action.Type {
		case "openPage":
			name, input = "WebFetch", map[string]any{"url": it.Action.URL, "prompt": ""}
		case "search":
			if len(it.Action.Queries) > 0 {
				input["query"] = strings.Join(it.Action.Queries, " | ")
			}
		case "findInPage":
			if it.Action.Pattern != "" {
				input["query"] = it.Action.Pattern
			}
		}
		var lines []string
		for _, r := range it.Results {
			s := r.Title
			if r.URL != "" {
				s += " — " + r.URL
			}
			if r.Snippet != "" {
				s += "\n  " + r.Snippet
			}
			lines = append(lines, strings.TrimSpace(s))
		}
		return [][]byte{
			codexToolUse(uid, ts, it.ID, name, input, marker),
			codexMessage("user", uid+"-r", ts, "user", []map[string]any{{
				"type": "tool_result", "tool_use_id": it.ID, "content": strings.Join(lines, "\n"),
			}}, marker),
		}
	case "SubAgentActivity":
		if it.Kind == "interacted" || it.AgentPath == "" {
			return nil
		}
		return one(codexEvent("system", uid, ts, map[string]any{
			"subtype":       "subagent",
			"content":       "Agent " + it.Kind + ": " + it.AgentPath,
			"status":        it.Kind,
			"agentPath":     it.AgentPath,
			"agentThreadId": it.AgentThreadID,
		}))
	case "EnteredReviewMode":
		return one(codexEvent("system", uid, ts, map[string]any{
			"subtype": "review", "content": "Review started: " + it.UserFacingHint,
		}))
	case "ExitedReviewMode":
		text := "Review finished"
		if it.ReviewOutput != nil {
			text += ": " + it.ReviewOutput.Correctness
			if it.ReviewOutput.Explanation != "" {
				text += "\n" + it.ReviewOutput.Explanation
			}
		}
		return one(codexEvent("system", uid, ts, map[string]any{"subtype": "review", "content": text}))
	}
	return nil
}

func codexChangeInput(fp string, ch codexFileChange) (string, map[string]any) {
	switch ch.Type {
	case "add":
		return "Write", map[string]any{"file_path": fp, "content": ch.Content}
	case "delete":
		return "Edit", map[string]any{"file_path": fp, "old_string": ch.Content, "new_string": ""}
	}
	old, nw := codexSplitDiff(ch.UnifiedDiff)
	in := map[string]any{"file_path": fp, "old_string": old, "new_string": nw, "diff": ch.UnifiedDiff}
	if ch.MovePath != "" {
		in["move_path"] = ch.MovePath
	}
	return "Edit", in
}

func codexSplitDiff(diff string) (string, string) {
	var old, nw []string
	for _, ln := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "@@"):
			if len(old) > 0 || len(nw) > 0 {
				old = append(old, ln)
				nw = append(nw, ln)
			}
		case strings.HasPrefix(ln, "-"):
			old = append(old, ln[1:])
		case strings.HasPrefix(ln, "+"):
			nw = append(nw, ln[1:])
		case strings.HasPrefix(ln, " "):
			old = append(old, ln[1:])
			nw = append(nw, ln[1:])
		case strings.HasPrefix(ln, "\\"):
		default:
			old = append(old, ln)
			nw = append(nw, ln)
		}
	}
	return strings.Join(old, "\n"), strings.Join(nw, "\n")
}

func codexResultFromStatus(id, status, stdout, stderr string) map[string]any {
	block := map[string]any{"type": "tool_result", "tool_use_id": id}
	switch status {
	case "completed", "":
		block["content"] = stdout
	case "declined":
		block["content"] = "Declined by user"
		block["is_error"] = true
	default:
		text := stderr
		if text == "" {
			text = stdout
		}
		if text == "" {
			text = status
		}
		block["content"] = text
		block["is_error"] = true
	}
	return block
}

func codexResponseItem(p *codexPayload, uid, ts string) [][]byte {
	switch p.Type {
	case "message":
		switch p.Role {
		case "user":
			text := codexJoinText(p.Content)
			if text == "" || strings.HasPrefix(text, "<environment_context>") {
				return nil
			}
			extra := map[string]any{}
			if strings.HasPrefix(text, "<") || strings.HasPrefix(text, "# AGENTS.md") {
				extra["isMeta"] = true
			}
			return one(codexMessage("user", uid, ts, "user", text, extra))
		case "developer":
			text := codexJoinText(p.Content)
			if strings.TrimSpace(text) == "" {
				return nil
			}
			for _, prefix := range codexDroppedDeveloper {
				if strings.HasPrefix(text, prefix) {
					return nil
				}
			}
			return one(codexMessage("user", uid, ts, "user", text, map[string]any{"isMeta": true}))
		case "assistant":
			var blocks []map[string]any
			for _, c := range p.Content {
				if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": c.Text})
				}
			}
			if len(blocks) == 0 {
				return nil
			}
			return one(codexMessage("assistant", uid, ts, "assistant", blocks, nil))
		}
	case "agent_message":
		body := strings.TrimSpace(codexAgentHdrRe.ReplaceAllString(codexJoinText(p.Content), ""))
		text := "**" + p.Author + " → " + p.Recipient + "**"
		if body != "" {
			text += "\n" + body
		}
		return one(codexMessage("assistant", uid, ts, "assistant",
			[]map[string]any{{"type": "text", "text": text}}, nil))
	case "reasoning":
		var blocks []map[string]any
		for _, c := range p.Summary {
			if strings.TrimSpace(c.Text) != "" {
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": c.Text})
			}
		}
		if len(blocks) == 0 {
			return nil
		}
		return one(codexMessage("assistant", uid, ts, "assistant", blocks, nil))
	case "function_call":
		name, input := codexToolInput(p.Name, p.Arguments)
		return one(codexToolUse(uid, ts, p.CallID, name, input, nil))
	case "custom_tool_call":
		name, input := codexCustomToolInput(p.Name, p.Input)
		return one(codexToolUse(uid, ts, p.CallID, name, input, nil))
	case "function_call_output", "custom_tool_call_output":
		block := codexResultBlock(p.CallID, p.Output)
		return one(codexMessage("user", uid, ts, "user", []map[string]any{block}, nil))
	}
	return nil
}

func codexToolInput(name, arguments string) (string, any) {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return name, map[string]any{"arguments": arguments}
	}
	switch name {
	case "exec_command":
		if cmd, ok := args["cmd"].(string); ok {
			return "Bash", codexBashInput(cmd, args)
		}
	case "request_user_input", "request_user_input_async":
		return "AskUserQuestion", codexQuestionsInput(args)
	case "send_message", "followup_task", "spawn_agent":
		if msg, ok := args["message"].(string); ok && codexEncryptedRe.MatchString(msg) {
			args["message"] = fmt.Sprintf("[encrypted message, %d chars]", len(msg))
			args["encrypted"] = true
		}
	case "sleep":
		if ms, ok := args["duration_ms"].(float64); ok {
			args["description"] = fmt.Sprintf("sleep %s", formatMs(int64(ms)))
		}
	}
	return name, args
}

func codexQuestionsInput(args map[string]any) map[string]any {
	raw, _ := args["questions"].([]any)
	var qs []map[string]any
	for _, q := range raw {
		m, ok := q.(map[string]any)
		if !ok {
			continue
		}
		out := map[string]any{"multiSelect": false}
		if s, ok := m["question"].(string); ok && s != "" {
			out["question"] = s
		} else if s, ok := m["title"].(string); ok {
			out["question"] = s
		}
		if s, ok := m["header"].(string); ok && s != "" {
			out["header"] = s
		} else if s, ok := m["id"].(string); ok {
			out["header"] = s
		}
		if s, ok := m["id"].(string); ok {
			out["id"] = s
		}
		var opts []map[string]any
		if list, ok := m["options"].([]any); ok {
			for _, o := range list {
				switch v := o.(type) {
				case string:
					opts = append(opts, map[string]any{"label": v})
				case map[string]any:
					opts = append(opts, v)
				}
			}
		}
		out["options"] = opts
		qs = append(qs, out)
	}
	return map[string]any{"questions": qs}
}

type codexCall struct {
	Name string
	Args any
	OK   bool
}

func codexScriptCalls(script string) []codexCall {
	var calls []codexCall
	for _, m := range codexToolRe.FindAllStringSubmatchIndex(script, -1) {
		c := codexCall{Name: script[m[2]:m[3]]}
		p := &jsParser{src: script, pos: m[1]}
		p.ws()
		if p.peek() == ')' {
			c.Args, c.OK = map[string]any{}, true
		} else if v, ok := p.value(); ok {
			c.Args, c.OK = v, true
		}
		calls = append(calls, c)
	}
	return calls
}

func codexCustomToolInput(name, input string) (string, any) {
	if name != "exec" {
		return name, map[string]any{"input": input}
	}
	calls := codexScriptCalls(input)
	if len(calls) == 1 && calls[0].OK {
		if n, in := codexSingleCall(calls[0]); n != "" {
			return n, in
		}
	}
	var names []string
	seen := map[string]bool{}
	for _, c := range calls {
		if !seen[c.Name] {
			seen[c.Name] = true
			names = append(names, c.Name)
		}
	}
	desc := strings.Join(names, ", ")
	if desc == "" {
		desc = "script"
	}
	return name, map[string]any{"code": input, "description": desc}
}

func codexSingleCall(c codexCall) (string, any) {
	args, _ := c.Args.(map[string]any)
	str := func(k string) string {
		s, _ := args[k].(string)
		return s
	}
	switch c.Name {
	case "exec_command":
		if cmd := str("cmd"); cmd != "" {
			return "Bash", codexBashInput(cmd, args)
		}
	case "apply_patch":
		if patch, ok := c.Args.(string); ok && patch != "" {
			return codexPatchInput(patch)
		}
	case "write_stdin":
		if args == nil {
			return "", nil
		}
		chars := str("chars")
		in := map[string]any{"command": chars, "description": fmt.Sprintf("stdin → session %v", args["session_id"])}
		if chars == "" {
			in["command"] = "(read output)"
		}
		return "Bash", in
	case "view_image":
		if fp := str("path"); fp != "" {
			return "Read", map[string]any{"file_path": fp}
		}
	case "web__run":
		if args == nil {
			return "", nil
		}
		if list, ok := args["search_query"].([]any); ok && len(list) > 0 {
			var qs []string
			for _, q := range list {
				if m, ok := q.(map[string]any); ok {
					if s, ok := m["q"].(string); ok {
						qs = append(qs, s)
					}
				}
			}
			return "WebSearch", map[string]any{"query": strings.Join(qs, " | ")}
		}
		if list, ok := args["open"].([]any); ok && len(list) > 0 {
			if m, ok := list[0].(map[string]any); ok {
				if ref, ok := m["ref_id"].(string); ok {
					return "WebFetch", map[string]any{"url": ref, "prompt": ""}
				}
			}
		}
		if list, ok := args["find"].([]any); ok && len(list) > 0 {
			if m, ok := list[0].(map[string]any); ok {
				if pat, ok := m["pattern"].(string); ok {
					return "WebSearch", map[string]any{"query": pat}
				}
			}
		}
		return "web__run", codexWithDescription(args, "web")
	case "clock__curr_time":
		return c.Name, codexWithDescription(args, "current time")
	case "get_goal":
		return c.Name, codexWithDescription(args, "get goal")
	case "update_goal":
		return c.Name, codexWithDescription(args, strings.TrimSpace("update goal "+str("status")))
	case "create_goal":
		return c.Name, codexWithDescription(args, firstLineEllipsis("create goal: "+str("objective"), 120))
	case "image_gen__imagegen":
		return c.Name, codexWithDescription(args, firstLineEllipsis("image: "+str("prompt"), 120))
	}
	return "", nil
}

func codexWithDescription(args map[string]any, desc string) map[string]any {
	out := map[string]any{"description": desc}
	for k, v := range args {
		out[k] = v
	}
	return out
}

func codexPatchInput(patch string) (string, any) {
	files := codexPatchFileRe.FindAllStringSubmatchIndex(patch, -1)
	if len(files) == 0 {
		return "Edit", map[string]any{"file_path": "", "old_string": "", "new_string": "", "patch": patch}
	}
	op, fp := patch[files[0][2]:files[0][3]], strings.TrimSpace(patch[files[0][4]:files[0][5]])
	end := len(patch)
	if len(files) > 1 {
		end = files[1][0]
	}
	body := patch[files[0][1]:end]
	body = strings.TrimSuffix(strings.TrimRight(body, "\n"), "*** End Patch")
	body = strings.TrimPrefix(body, "\n")
	in := map[string]any{"file_path": fp, "patch": patch}
	if len(files) > 1 {
		in["description"] = fmt.Sprintf("%s (+%d more files)", path.Base(fp), len(files)-1)
	}
	switch op {
	case "Add":
		var lines []string
		for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			lines = append(lines, strings.TrimPrefix(ln, "+"))
		}
		in["content"] = strings.Join(lines, "\n")
		return "Write", in
	case "Delete":
		in["old_string"], in["new_string"] = "(deleted)", ""
		return "Edit", in
	}
	if m := codexMovePathRe.FindStringSubmatch(body); m != nil {
		in["move_path"] = strings.TrimSpace(m[1])
		body = codexMovePathRe.ReplaceAllString(body, "")
	}
	in["old_string"], in["new_string"] = codexSplitDiff(body)
	return "Edit", in
}

func codexBashInput(cmd string, args map[string]any) map[string]any {
	in := map[string]any{"command": cmd}
	if wd, ok := args["workdir"].(string); ok && wd != "" {
		in["workdir"] = wd
	}
	if j, ok := args["justification"].(string); ok && j != "" {
		in["description"] = j
	}
	return in
}

func codexResultBlock(callID string, raw json.RawMessage) map[string]any {
	block := map[string]any{"type": "tool_result", "tool_use_id": callID}
	isErr := false
	var parts []string
	items := codexOutputItems(raw)
	for i, c := range items {
		switch c.Type {
		case "input_image", "image", "output_image":
			parts = append(parts, "[image]")
			continue
		case "encrypted_content":
			parts = append(parts, "[encrypted]")
			continue
		}
		text := c.Text
		if i == 0 && codexHeaderRe.MatchString(text) {
			if strings.HasPrefix(text, "Script failed") {
				isErr = true
			}
			if strings.HasPrefix(text, "Script completed") {
				continue
			}
			text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\nOutput:")
		}
		if strings.HasPrefix(text, "Script error:") || strings.HasPrefix(text, "aborted by user after") {
			isErr = true
		}
		if m := codexExitRe.FindStringSubmatch(text); m != nil {
			if code, _ := strconv.Atoi(m[1]); code != 0 {
				isErr = true
			}
		}
		if t, failed, ok := codexUnwrapResult(text); ok {
			text = t
			isErr = isErr || failed
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	block["content"] = strings.Join(parts, "\n")
	if isErr {
		block["is_error"] = true
	}
	return block
}

func codexOutputItems(raw json.RawMessage) []codexContent {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return []codexContent{{Type: "input_text", Text: s}}
	}
	var items []codexContent
	if err := json.Unmarshal(raw, &items); err != nil {
		return []codexContent{{Type: "input_text", Text: string(raw)}}
	}
	return items
}

func codexUnwrapResult(text string) (string, bool, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "{") {
		return "", false, false
	}
	var m map[string]any
	if json.Unmarshal([]byte(t), &m) != nil {
		return "", false, false
	}
	return codexUnwrapMap(m, 0)
}

func codexUnwrapMap(m map[string]any, depth int) (string, bool, bool) {
	if depth > 3 {
		return "", false, false
	}
	if status, _ := m["status"].(string); status == "rejected" {
		reason := fmt.Sprint(m["reason"])
		return "Script error: " + reason, true, true
	}
	if answers, ok := m["answers"].(map[string]any); ok && len(m) == 1 {
		ids := make([]string, 0, len(answers))
		for id := range answers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var lines []string
		for _, id := range ids {
			a, _ := answers[id].(map[string]any)
			var picked []string
			if list, ok := a["answers"].([]any); ok {
				for _, v := range list {
					picked = append(picked, fmt.Sprint(v))
				}
			}
			lines = append(lines, id+": "+strings.Join(picked, "; "))
		}
		return strings.Join(lines, "\n"), false, true
	}
	for _, k := range []string{"value", "result"} {
		if inner, ok := m[k].(map[string]any); ok {
			if _, hasOut := inner["output"]; hasOut || inner["status"] != nil || inner["value"] != nil {
				return codexUnwrapMap(inner, depth+1)
			}
		}
	}
	out, hasOut := m["output"].(string)
	_, hasChunk := m["chunk_id"]
	_, hasExit := m["exit_code"]
	_, hasSession := m["session_id"]
	if !hasOut || !(hasChunk || hasExit || hasSession) {
		return "", false, false
	}
	failed := false
	if code, ok := m["exit_code"].(float64); ok && code != 0 {
		failed = true
		out = fmt.Sprintf("Process exited with code %d\n%s", int(code), out)
	}
	if !hasExit && hasSession {
		out = fmt.Sprintf("[session %v running]\n%s", m["session_id"], out)
	}
	return strings.TrimRight(out, "\n"), failed, true
}

func codexJoinText(items []codexContent) string {
	var parts []string
	for _, c := range items {
		switch c.Type {
		case "input_text", "output_text", "text", "summary_text":
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		case "input_image", "image":
			parts = append(parts, "[image]")
		case "encrypted_content":
			parts = append(parts, "[encrypted]")
		}
	}
	return strings.Join(parts, "\n")
}

func codexToolUse(uid, ts, callID, name string, input any, extra map[string]any) []byte {
	return codexMessage("assistant", uid, ts, "assistant", []map[string]any{{
		"type": "tool_use", "id": callID, "name": name, "input": input,
	}}, extra)
}

func codexMessage(typ, uid, ts, role string, content any, extra map[string]any) []byte {
	ev := map[string]any{
		"type":        typ,
		"uuid":        uid,
		"parentUuid":  nil,
		"isSidechain": false,
		"timestamp":   ts,
		"message":     map[string]any{"role": role, "content": content},
	}
	for k, v := range extra {
		ev[k] = v
	}
	b, _ := json.Marshal(ev)
	return b
}

func codexEvent(typ, uid, ts string, fields map[string]any) []byte {
	ev := map[string]any{"type": typ, "uuid": uid, "timestamp": ts}
	for k, v := range fields {
		ev[k] = v
	}
	b, _ := json.Marshal(ev)
	return b
}

func one(b []byte) [][]byte { return [][]byte{b} }

type jsParser struct {
	src   string
	pos   int
	depth int
}

const jsDeclPattern = `(?:const|let|var)\s+%s\s*=\s*`

func (p *jsParser) peek() byte {
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *jsParser) ws() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case strings.HasPrefix(p.src[p.pos:], "//"):
			if nl := strings.IndexByte(p.src[p.pos:], '\n'); nl >= 0 {
				p.pos += nl + 1
			} else {
				p.pos = len(p.src)
			}
		case strings.HasPrefix(p.src[p.pos:], "/*"):
			if end := strings.Index(p.src[p.pos+2:], "*/"); end >= 0 {
				p.pos += end + 4
			} else {
				p.pos = len(p.src)
			}
		default:
			return
		}
	}
}

func (p *jsParser) value() (any, bool) {
	p.ws()
	c := p.peek()
	switch {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"' || c == '\'' || c == '`':
		return p.str()
	case c == '-' || c == '+' || (c >= '0' && c <= '9'):
		return p.number()
	case c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		return p.ident()
	}
	return nil, false
}

func (p *jsParser) object() (any, bool) {
	p.pos++
	out := map[string]any{}
	for {
		p.ws()
		switch p.peek() {
		case '}':
			p.pos++
			return out, true
		case ',':
			p.pos++
			continue
		case 0:
			return nil, false
		}
		var key string
		if c := p.peek(); c == '"' || c == '\'' {
			k, ok := p.str()
			if !ok {
				return nil, false
			}
			key = k.(string)
		} else {
			start := p.pos
			for p.pos < len(p.src) && isIdentByte(p.src[p.pos]) {
				p.pos++
			}
			if start == p.pos {
				return nil, false
			}
			key = p.src[start:p.pos]
		}
		p.ws()
		if p.peek() != ':' {
			v, ok := p.resolve(key)
			if !ok {
				return nil, false
			}
			out[key] = v
			continue
		}
		p.pos++
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		out[key] = v
	}
}

func (p *jsParser) array() (any, bool) {
	p.pos++
	out := []any{}
	for {
		p.ws()
		switch p.peek() {
		case ']':
			p.pos++
			return out, true
		case ',':
			p.pos++
			continue
		case 0:
			return nil, false
		}
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		out = append(out, v)
	}
}

func (p *jsParser) str() (any, bool) {
	quote := p.src[p.pos]
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == quote:
			p.pos++
			return sb.String(), true
		case c == '\\':
			p.pos++
			if p.pos >= len(p.src) {
				return nil, false
			}
			e := p.src[p.pos]
			p.pos++
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'v':
				sb.WriteByte('\v')
			case '0':
				sb.WriteByte(0)
			case '\n':
			case 'x':
				if p.pos+2 > len(p.src) {
					return nil, false
				}
				n, err := strconv.ParseUint(p.src[p.pos:p.pos+2], 16, 8)
				if err != nil {
					return nil, false
				}
				sb.WriteRune(rune(n))
				p.pos += 2
			case 'u':
				var hex string
				if p.peek() == '{' {
					end := strings.IndexByte(p.src[p.pos:], '}')
					if end < 0 {
						return nil, false
					}
					hex = p.src[p.pos+1 : p.pos+end]
					p.pos += end + 1
				} else {
					if p.pos+4 > len(p.src) {
						return nil, false
					}
					hex = p.src[p.pos : p.pos+4]
					p.pos += 4
				}
				n, err := strconv.ParseUint(hex, 16, 32)
				if err != nil {
					return nil, false
				}
				sb.WriteRune(rune(n))
			default:
				sb.WriteByte(e)
			}
		case quote == '`' && c == '$' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '{':
			return nil, false
		default:
			sb.WriteByte(c)
			p.pos++
		}
	}
	return nil, false
}

func (p *jsParser) number() (any, bool) {
	start := p.pos
	if c := p.peek(); c == '-' || c == '+' {
		p.pos++
	}
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == 'x' || c == '_' ||
			(c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			p.pos++
			continue
		}
		break
	}
	lit := strings.ReplaceAll(p.src[start:p.pos], "_", "")
	if f, err := strconv.ParseFloat(lit, 64); err == nil {
		return f, true
	}
	if n, err := strconv.ParseInt(lit, 0, 64); err == nil {
		return float64(n), true
	}
	return nil, false
}

func (p *jsParser) ident() (any, bool) {
	start := p.pos
	for p.pos < len(p.src) && isIdentByte(p.src[p.pos]) {
		p.pos++
	}
	name := p.src[start:p.pos]
	switch name {
	case "true":
		return true, true
	case "false":
		return false, true
	case "null", "undefined":
		return nil, true
	}
	return p.resolve(name)
}

func (p *jsParser) resolve(name string) (any, bool) {
	if p.depth > 2 {
		return nil, false
	}
	re, err := regexp.Compile(fmt.Sprintf(jsDeclPattern, regexp.QuoteMeta(name)))
	if err != nil {
		return nil, false
	}
	m := re.FindStringIndex(p.src)
	if m == nil {
		return nil, false
	}
	sub := &jsParser{src: p.src, pos: m[1], depth: p.depth + 1}
	v, ok := sub.value()
	if !ok {
		return nil, false
	}
	sub.ws()
	if sub.peek() == '+' {
		return nil, false
	}
	return v, true
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
