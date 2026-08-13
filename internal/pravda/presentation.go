package pravda

var toolMeta = map[string]struct {
	Icon string
	Cls  string
}{
	"Bash":         {"🖥️", "op-bash"},
	"Read":         {"📄", "op-read"},
	"Write":        {"✏️", "op-write"},
	"Edit":         {"✂️", "op-edit"},
	"MultiEdit":    {"✂️", "op-edit"},
	"LS":           {"📁", "op-ls"},
	"Grep":         {"🔎", "op-grep"},
	"Glob":         {"🔍", "op-glob"},
	"Agent":        {"🤖", "op-task"},
	"Task":         {"🤖", "op-task"},
	"TodoWrite":    {"✅", "op-todowrite"},
	"TaskCreate":   {"➕", "op-task"},
	"TaskUpdate":   {"🔄", "op-task"},
	"TaskGet":      {"🔍", "op-task"},
	"TaskList":     {"📋", "op-task"},
	"ToolSearch":   {"🧰", "op-system"},
	"WebFetch":     {"🌐", "op-system"},
	"WebSearch":    {"🔍", "op-system"},
	"NotebookEdit": {"📓", "op-system"},
}

func (e *Entry) Icon() string {
	if e.Kind == KindTool {
		if m, ok := toolMeta[e.Name]; ok {
			return m.Icon
		}
		return "🔧"
	}
	switch e.Kind {
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
		}
		return "📎"
	case KindAgentResult:
		if e.Status == "completed" {
			return "✅"
		}
		return "⚠️"
	case KindSystem:
		return "⚙️"
	}
	return "❓"
}

func (e *Entry) Label() string {
	if e.Kind == KindTool {
		return e.Name
	}
	switch e.Kind {
	case KindUser:
		return "User"
	case KindMessage:
		return "Claude"
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
		}
		return "Attachment"
	case KindAgentResult:
		return "Agent result"
	case KindSystem:
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
	case KindUser:
		return "op-user"
	case KindInterrupt:
		return "op-warn"
	case KindAgentResult:
		if e.Status == "completed" {
			return "op-task"
		}
		return "op-warn"
	}
	return "op-system"
}
