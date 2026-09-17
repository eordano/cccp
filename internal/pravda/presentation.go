package pravda

import "strings"

var toolMeta = map[string]struct {
	Icon string
	Cls  string
}{
	"Bash":                {"🖥️", "op-bash"},
	"exec":                {"🧩", "op-bash"},
	"send_message":        {"📨", "op-agent"},
	"followup_task":       {"📨", "op-agent"},
	"spawn_agent":         {"🚀", "op-agent"},
	"wait_agent":          {"⏳", "op-agent"},
	"wait":                {"⏳", "op-agent"},
	"list_agents":         {"👥", "op-agent"},
	"interrupt_agent":     {"🛑", "op-agent"},
	"sleep":               {"💤", "op-bash"},
	"clock__curr_time":    {"🕒", "op-bash"},
	"get_goal":            {"🎯", "op-bash"},
	"update_goal":         {"🎯", "op-bash"},
	"create_goal":         {"🎯", "op-bash"},
	"image_gen__imagegen": {"🖼️", "op-bash"},
	"web__run":            {"🌐", "op-web"},
	"Read":                {"📄", "op-read"},
	"Write":               {"✏️", "op-write"},
	"Edit":                {"✂️", "op-edit"},
	"MultiEdit":           {"✂️", "op-edit"},
	"LS":                  {"📁", "op-ls"},
	"Grep":                {"🔎", "op-grep"},
	"Glob":                {"🔍", "op-glob"},
	"Agent":               {"🤖", "op-task"},
	"Task":                {"🤖", "op-task"},
	"TodoWrite":           {"✅", "op-todowrite"},
	"TaskCreate":          {"➕", "op-task"},
	"TaskUpdate":          {"🔄", "op-task"},
	"TaskGet":             {"🔍", "op-task"},
	"TaskList":            {"📋", "op-task"},
	"ToolSearch":          {"🧰", "op-system"},
	"WebFetch":            {"🌐", "op-system"},
	"WebSearch":           {"🔍", "op-system"},
	"NotebookEdit":        {"📓", "op-system"},
	"Artifact":            {"🎨", "op-write"},
	"AskUserQuestion":     {"❓", "op-user"},
	"Workflow":            {"🕸️", "op-task"},
	"Skill":               {"🎓", "op-system"},
	"SendMessage":         {"📨", "op-task"},
	"SendUserFile":        {"📤", "op-user"},
	"Monitor":             {"👁️", "op-system"},
	"TaskStop":            {"🛑", "op-task"},
	"TaskOutput":          {"📬", "op-task"},
	"ListAgents":          {"📇", "op-task"},
	"ScheduleWakeup":      {"⏰", "op-system"},
	"EnterPlanMode":       {"📋", "op-system"},
	"ExitPlanMode":        {"📋", "op-system"},
	"EnterWorktree":       {"🌿", "op-system"},
	"ExitWorktree":        {"🌿", "op-system"},
	"ReportFindings":      {"🐞", "op-warn"},
	"LSP":                 {"🧭", "op-system"},
	"StructuredOutput":    {"📦", "op-write"},
	"PushNotification":    {"🔔", "op-user"},
}

func (e *Entry) Icon() string {
	if e.Kind == KindTool {
		if m, ok := toolMeta[e.Name]; ok {
			return m.Icon
		}
		if strings.HasPrefix(e.Name, "mcp__") {
			return "🔌"
		}
		return "🔧"
	}
	switch e.Kind {
	case KindWorkflow:
		if e.Op == "result" {
			return "🏁"
		}
		return "🚀"
	case "child-session":
		return "🧵"
	case KindUser:
		return "👤"
	case KindMessage:
		return "💬"
	case KindThinking:
		return "💭"
	case KindInterrupt:
		return "⚠️"
	case KindCommand:
		return "⌘"
	case KindCommandOutput:
		return "📤"
	case KindMeta:
		return "📝"
	case KindTurnDuration:
		return "⏱️"
	case KindPermissionMode:
		return "🔐"
	case KindQueueOperation:
		switch e.Op {
		case "enqueue":
			return "📥"
		case "dequeue":
			return "▶️"
		case "remove":
			return "🗑"
		case "popAll":
			return "⏬"
		}
		return "📥"
	case KindAttachment:
		switch e.Subtype {
		case "file":
			return "📎"
		case "compact_file_reference":
			return "📄"
		case "directory":
			return "📁"
		case "plan_mode_exit":
			return "📋"
		case "queued_command":
			return "⏸"
		case "task_reminder":
			return "🔔"
		case "skill_listing":
			return "🎓"
		case "deferred_tools_delta":
			return "🧩"
		case "total_tokens_reminder":
			return "🧮"
		case "hook_success", "hook_additional_context", "hook_system_message":
			return "🪝"
		case "date_change":
			return "📅"
		case "command_permissions":
			return "🔑"
		case "auto_mode":
			return "🤖"
		case "goal_status":
			return "🎯"
		case "nested_memory":
			return "🧠"
		case "invoked_skills":
			return "🎓"
		case "mcp_instructions_delta":
			return "🔌"
		case "agent_listing_delta":
			return "🤖"
		case "plan_mode":
			return "📋"
		case "remote_session_change":
			return "🔗"
		case "instructions":
			return "📜"
		case "prompt_snapshot":
			return "🧾"
		case "deferred_tools_record":
			return "🧩"
		case "read_truncation_notice":
			return "✂️"
		case "auto_mode_exit":
			return "🤖"
		case "dynamic_skill":
			return "🎓"
		case "thinking_stripped":
			return "🧹"
		case "structured_output":
			return "📦"
		}
		return "📎"
	case KindAgentResult:
		if e.Status == "completed" {
			return "✅"
		}
		return "⚠️"
	case KindBashInput:
		return "🖥️"
	case KindBashOutput:
		return "📤"
	case KindMode:
		if e.Subtype == "model-change" {
			return "🧠"
		}
		return "🎚️"
	case KindPRLink:
		return "🔗"
	case KindFrameLink:
		return "🖼️"
	case KindSystem:
		switch e.Subtype {
		case "away_summary":
			return "💤"
		case "compact_boundary":
			return "🗜️"
		case "stop_hook_summary":
			return "🪝"
		case "subagent":
			return "🧵"
		case "review":
			return "🔍"
		case "model_refusal_no_fallback", "model_refusal_fallback":
			return "⛔"
		case "model_fallback":
			return "🔀"
		case "bridge_status":
			return "📡"
		case "informational":
			return "ℹ️"
		case "goal":
			return "🎯"
		}
		return "⚙️"
	}
	return "❓"
}

func (e *Entry) Label() string {
	if e.Kind == KindTool {
		if strings.HasPrefix(e.Name, "mcp__") {
			if parts := strings.SplitN(e.Name, "__", 3); len(parts) == 3 {
				return parts[2]
			}
		}
		return e.Name
	}
	switch e.Kind {
	case KindUser:
		return "User"
	case KindMessage:
		return agentDisplayName(e.Agent)
	case KindWorkflow:
		if e.Op == "result" {
			return "Agent result"
		}
		return "Agent started"
	case "child-session":
		return "Sub-session"
	case KindThinking:
		return "Thinking"
	case KindInterrupt:
		return "Interrupt"
	case KindCommand:
		return "Slash Command"
	case KindCommandOutput:
		return "Output"
	case KindMeta:
		return "Meta"
	case KindTurnDuration:
		return "Turn"
	case KindPermissionMode:
		return "Permission"
	case KindQueueOperation:
		switch e.Op {
		case "enqueue":
			return "Enqueue"
		case "dequeue":
			return "Dequeue"
		case "remove":
			return "Remove"
		case "popAll":
			return "Clear queue"
		}
		return "Queue"
	case KindAttachment:
		switch e.Subtype {
		case "file":
			return "File"
		case "compact_file_reference":
			return "File ref"
		case "directory":
			return "Directory"
		case "plan_mode_exit":
			return "Plan mode"
		case "queued_command":
			return "Queued"
		case "task_reminder":
			return "Task Reminder"
		case "skill_listing":
			return "Skills"
		case "deferred_tools_delta":
			return "Tools Delta"
		case "total_tokens_reminder":
			return "Tokens"
		case "hook_success":
			return "Hook"
		case "hook_additional_context":
			return "Hook ctx"
		case "hook_system_message":
			return "Hook msg"
		case "date_change":
			return "Date"
		case "command_permissions":
			return "Permissions"
		case "auto_mode":
			return "Auto mode"
		case "goal_status":
			return "Goal"
		case "nested_memory":
			return "Memory"
		case "invoked_skills":
			return "Skills"
		case "mcp_instructions_delta":
			return "MCP Delta"
		case "agent_listing_delta":
			return "Agents Delta"
		case "plan_mode":
			return "Plan mode"
		case "remote_session_change":
			return "Session"
		case "instructions":
			return "Instructions"
		case "prompt_snapshot":
			return "System prompt"
		case "deferred_tools_record":
			return "Tool schemas"
		case "read_truncation_notice":
			return "Truncated"
		case "auto_mode_exit":
			return "Auto mode"
		case "dynamic_skill":
			return "Skill"
		case "thinking_stripped":
			return "Thinking"
		case "structured_output":
			return "Structured"
		}
		return "Attachment"
	case KindAgentResult:
		return "Agent result"
	case KindBashInput:
		return "! bash"
	case KindBashOutput:
		return "Output"
	case KindMode:
		if e.Subtype == "model-change" {
			return "Model"
		}
		return "Mode"
	case KindPRLink:
		return "PR"
	case KindFrameLink:
		return "Artifact"
	case KindSystem:
		switch e.Subtype {
		case "away_summary":
			return "Recap"
		case "compact_boundary":
			return "Compacted"
		case "stop_hook_summary":
			return "Stop hooks"
		case "subagent":
			return "Sub-agent"
		case "review":
			return "Review mode"
		case "model_refusal_no_fallback", "model_refusal_fallback":
			return "Refused"
		case "model_fallback":
			return "Fallback"
		case "bridge_status":
			return "Remote"
		case "informational":
			return "Info"
		case "goal":
			return "Goal"
		}
		return "System"
	}
	return e.Kind
}

func (e *Entry) OpClass() string {
	if e.Kind == KindTool {
		if m, ok := toolMeta[e.Name]; ok {
			return m.Cls
		}
		return "op-system"
	}
	switch e.Kind {
	case KindUser, KindBashInput:
		return "op-user"
	case KindBashOutput:
		return "op-bash"
	case KindInterrupt:
		return "op-warn"
	case KindPRLink, KindFrameLink:
		return "op-write"
	case KindSystem:
		switch e.Subtype {
		case "model_refusal_no_fallback", "model_refusal_fallback", "model_fallback":
			return "op-warn"
		}
		if e.Level == "warning" || e.Level == "error" {
			return "op-warn"
		}
	case KindTurnDuration:
		if e.Level == "error" {
			return "op-warn"
		}
	case KindAttachment:
		if e.Subtype == "read_truncation_notice" {
			return "op-warn"
		}
	case KindAgentResult:
		if e.Status == "completed" {
			return "op-task"
		}
		return "op-warn"
	}
	return "op-system"
}
