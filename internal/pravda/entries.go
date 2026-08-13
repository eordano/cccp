package pravda

import "encoding/json"

const (
	KindUser           = "user"
	KindMessage        = "message"
	KindThinking       = "thinking"
	KindTool           = "tool"
	KindInterrupt      = "interrupt"
	KindCommand        = "command"
	KindCommandOutput  = "command-output"
	KindMeta           = "meta"
	KindAgentResult    = "agent-result"
	KindTurnDuration   = "turn_duration"
	KindPermissionMode = "permission-mode"
	KindQueueOperation = "queue-operation"
	KindAttachment     = "attachment"
	KindSystem         = "system"
	KindUnknown        = "unknown"
)

type RawEvent struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	ParentUUID  string `json:"parentUuid"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	CWD         string `json:"cwd"`
	SessionID   string `json:"sessionId"`

	Subtype      string `json:"subtype"`
	DurationMs   int64  `json:"durationMs"`
	MessageCount int    `json:"messageCount"`

	PermissionMode string          `json:"permissionMode"`
	Operation      string          `json:"operation"`
	Content        json.RawMessage `json:"content"`

	Attachment json.RawMessage `json:"attachment"`

	Message *Message `json:"message"`

	ToolUseResult json.RawMessage `json:"toolUseResult"`

	queueCtx *QueueCtx `json:"-"`

	Raw json.RawMessage `json:"-"`
}

type Message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type ContentItem struct {
	Type string `json:"type"`

	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string          `json:"tool_use_id,omitempty"`
	RContent  json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	ToolName string `json:"tool_name,omitempty"`
}

type QueueCtx struct {
	Action   string
	Item     string
	QueuedAt string
	Items    []QueuedItem
	Depth    int
	Position int
}

type QueuedItem struct {
	Content string `json:"content"`
	TS      string `json:"ts"`
}

type AgentRef struct {
	AgentID           string          `json:"agentId"`
	Status            string          `json:"status"`
	IsAsync           bool            `json:"isAsync"`
	Description       string          `json:"description"`
	TotalDurationMs   int64           `json:"totalDurationMs"`
	TotalTokens       int64           `json:"totalTokens"`
	TotalToolUseCount int             `json:"totalToolUseCount"`
	ToolStats         json.RawMessage `json:"toolStats"`
	OutputFile        string          `json:"outputFile"`
}

type Entry struct {
	Kind  string `json:"kind"`
	TS    string `json:"ts,omitempty"`
	Index int    `json:"index,omitempty"`

	Text string `json:"text,omitempty"`
	CWD  string `json:"cwd,omitempty"`

	HasBody bool `json:"hasBody,omitempty"`

	CommandName    string `json:"commandName,omitempty"`
	CommandMessage string `json:"commandMessage,omitempty"`
	CommandArgs    string `json:"commandArgs,omitempty"`

	AgentID    string `json:"agentId,omitempty"`
	ToolUseID  string `json:"toolUseId,omitempty"`
	Status     string `json:"status,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Result     string `json:"result,omitempty"`
	OutputFile string `json:"outputFile,omitempty"`

	Subtype string          `json:"subtype,omitempty"`
	Attach  json.RawMessage `json:"attachment,omitempty"`

	DurationMs   int64 `json:"durationMs,omitempty"`
	MessageCount int   `json:"messageCount,omitempty"`

	Mode string `json:"mode,omitempty"`

	Op       string       `json:"op,omitempty"`
	Item     string       `json:"item,omitempty"`
	QueuedAt string       `json:"queuedAt,omitempty"`
	Items    []QueuedItem `json:"items,omitempty"`
	Depth    int          `json:"depth,omitempty"`
	Position int          `json:"position,omitempty"`

	Name          string                     `json:"name,omitempty"`
	ID            string                     `json:"id,omitempty"`
	Input         json.RawMessage            `json:"input,omitempty"`
	InputMap      map[string]json.RawMessage `json:"inputMap,omitempty"`
	ResultContent json.RawMessage            `json:"resultContent,omitempty"`
	HasResult     bool                       `json:"hasResult,omitempty"`
	IsError       bool                       `json:"isError,omitempty"`
	ToolUseResult json.RawMessage            `json:"toolUseResult,omitempty"`
	AgentRef      *AgentRef                  `json:"agentRef,omitempty"`
	SubAgent      []Entry                    `json:"subAgent,omitempty"`

	Data json.RawMessage `json:"data,omitempty"`

	SubAgentPath string `json:"subAgentPath,omitempty"`
	ParentPath   string `json:"parentPath,omitempty"`

	RawSource  *RawEvent    `json:"-"`
	RawToolUse *ContentItem `json:"-"`
	RawResult  *RawEvent    `json:"-"`
}

func (e *Entry) TemplateName() string {
	if e.Kind == KindTool {
		return "tool-" + e.Name
	}
	if e.Kind == KindAttachment {
		return "attachment-" + e.Subtype
	}
	return "event-" + e.Kind
}
