package pravda

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type modelSpan struct {
	Model  string
	Effort string
	Index  int
	Turns  int
}

type modeSpan struct {
	Kind  string
	Value string
	Index int
}

type tokenTotals struct {
	Model         string
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	Cost          float64
	Turns         int
}

func (t tokenTotals) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheCreation }

type toolCount struct {
	Name  string
	Count int
}

type sessionEnv struct {
	Models   []modelSpan
	Version  string
	Platform string
	OS       string
	Shell    string
	GitRepo  bool
	Worktree bool
	Skills   []string
	Invoked  []string
	MCP      []string
	Hooks    []toolCount
	Modes    []modeSpan
	Tokens   []tokenTotals
	Total    tokenTotals
	Reported float64
	Tools    []toolCount
	ToolUses int
}

func (e *sessionEnv) Empty() bool {
	return e == nil || (len(e.Models) == 0 && len(e.Skills) == 0 && len(e.MCP) == 0 && len(e.Hooks) == 0 && e.Total.Turns == 0 && len(e.Tools) == 0)
}

func (e *sessionEnv) Summary() string {
	var parts []string
	distinct := map[string]bool{}
	for _, m := range e.Models {
		distinct[m.Model] = true
	}
	if n := len(distinct); n == 1 {
		parts = append(parts, e.Models[0].Model)
	} else if n > 1 {
		parts = append(parts, strconv.Itoa(n)+" models")
	}
	if e.Total.Turns > 0 {
		parts = append(parts, fmtTokens(e.Total.Total())+" tokens")
	}
	if c := e.Cost(); c > 0 {
		parts = append(parts, fmtUSD(c))
	}
	if e.ToolUses > 0 {
		parts = append(parts, strconv.Itoa(e.ToolUses)+" tool calls")
	}
	if len(e.Skills) > 0 {
		parts = append(parts, strconv.Itoa(len(e.Skills))+" skills")
	}
	if len(e.MCP) > 0 {
		parts = append(parts, strconv.Itoa(len(e.MCP))+" MCP")
	}
	if len(e.Hooks) > 0 {
		parts = append(parts, strconv.Itoa(len(e.Hooks))+" hooks")
	}
	return strings.Join(parts, " · ")
}

func (e *sessionEnv) Cost() float64 {
	if e.Reported > 0 {
		return e.Reported
	}
	return e.Total.Cost
}

type envAttachment struct {
	Type       string   `json:"type"`
	Names      []string `json:"names"`
	AddedNames []string `json:"addedNames"`
	HookName   string   `json:"hookName"`
	Skills     []struct {
		Name string `json:"name"`
	} `json:"skills"`
	Snapshot struct {
		Platform   string `json:"platform"`
		OSVersion  string `json:"osVersion"`
		Shell      string `json:"shell"`
		IsGitRepo  bool   `json:"isGitRepo"`
		IsWorktree bool   `json:"isWorktree"`
	} `json:"snapshot"`
	Identity struct {
		MarketingName string `json:"marketingName"`
	} `json:"identity"`
}

type costState struct {
	TotalCostUSD float64 `json:"totalCostUSD"`
	ModelUsage   map[string]struct {
		Input         int64   `json:"inputTokens"`
		Output        int64   `json:"outputTokens"`
		CacheRead     int64   `json:"cacheReadInputTokens"`
		CacheCreation int64   `json:"cacheCreationInputTokens"`
		Cost          float64 `json:"costUSD"`
	} `json:"modelUsage"`
}

type stringSet struct {
	seen  map[string]bool
	order []string
}

func (s *stringSet) add(v string) {
	if v == "" {
		return
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if !s.seen[v] {
		s.seen[v] = true
		s.order = append(s.order, v)
	}
}

func buildEnv(sess *Session) *sessionEnv {
	env := &sessionEnv{}
	rawIndex := map[*RawEvent]int{}
	tools := map[string]int{}
	var skills, invoked, mcp stringSet
	hooks := map[string]int{}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.RawSource != nil {
			if _, ok := rawIndex[e.RawSource]; !ok {
				rawIndex[e.RawSource] = e.Index
			}
		}
		switch e.Kind {
		case KindTool:
			tools[e.Name]++
			env.ToolUses++
			if strings.HasPrefix(e.Name, "mcp__") {
				if parts := strings.SplitN(e.Name, "__", 3); len(parts) == 3 {
					mcp.add(parts[1])
				}
			}
			if e.Name == "Skill" {
				var skill string
				_ = json.Unmarshal(e.InputMap["skill"], &skill)
				invoked.add(skill)
			}
		case KindMode:
			env.Modes = append(env.Modes, modeSpan{Kind: "mode", Value: e.Mode, Index: e.Index})
		case KindPermissionMode:
			env.Modes = append(env.Modes, modeSpan{Kind: "permission", Value: e.Mode, Index: e.Index})
		}
	}
	perModel := map[string]*tokenTotals{}
	var modelOrder []string
	addUsage := func(model string, u *TokenUsage, cost float64) {
		if (u == nil && cost == 0) || model == "<synthetic>" {
			return
		}
		if model == "" {
			model = "unknown"
		}
		t := perModel[model]
		if t == nil {
			t = &tokenTotals{Model: model}
			perModel[model] = t
			modelOrder = append(modelOrder, model)
		}
		if u != nil {
			t.Input += u.Input
			t.Output += u.Output
			t.CacheRead += u.CacheRead
			t.CacheCreation += u.CacheCreation
		}
		t.Cost += cost
		t.Turns++
	}
	var reported *costState
	lastModel, lastEffort := "", ""
	var lastUsage *RawEvent
	seenMsg := map[string]bool{}
	for _, e := range sess.Events {
		switch e.Type {
		case "assistant":
			if e.Message != nil && e.Message.Model != "<synthetic>" {
				if e.Message.Model != "" && (e.Message.Model != lastModel || e.Effort != lastEffort) {
					lastModel, lastEffort = e.Message.Model, e.Effort
					env.Models = append(env.Models, modelSpan{Model: lastModel, Effort: lastEffort, Index: rawIndex[e]})
				}
				if e.Message.ID != "" && seenMsg[e.Message.ID] {
					break
				}
				seenMsg[e.Message.ID] = true
				if len(env.Models) > 0 && e.Message.Usage != nil {
					env.Models[len(env.Models)-1].Turns++
				}
				addUsage(e.Message.Model, e.Message.Usage, 0)
			}
		case "usage":
			if lastUsage != nil && lastUsage.Model == e.Model && lastUsage.CostUSD == e.CostUSD && lastUsage.Usage != nil && e.Usage != nil && *lastUsage.Usage == *e.Usage {
				continue
			}
			lastUsage = e
			addUsage(e.Model, e.Usage, e.CostUSD)
		case "model-change":
			if e.Model != "" && (e.Model != lastModel || e.Effort != lastEffort) {
				lastModel, lastEffort = e.Model, e.Effort
				env.Models = append(env.Models, modelSpan{Model: lastModel, Effort: lastEffort, Index: rawIndex[e]})
			}
		case "cost-state":
			var cs costState
			if json.Unmarshal(e.Raw, &cs) == nil && (cs.TotalCostUSD > 0 || len(cs.ModelUsage) > 0) {
				reported = &cs
			}
		case "attachment":
			var a envAttachment
			if json.Unmarshal(e.Attachment, &a) != nil {
				continue
			}
			switch a.Type {
			case "skill_listing":
				for _, n := range a.Names {
					skills.add(n)
				}
			case "invoked_skills":
				for _, s := range a.Skills {
					invoked.add(s.Name)
				}
			case "mcp_instructions_delta":
				for _, n := range a.AddedNames {
					mcp.add(n)
				}
			case "hook_success", "hook_additional_context", "hook_system_message":
				if a.HookName != "" {
					hooks[a.HookName]++
				}
			case "environment":
				env.Platform = a.Snapshot.Platform
				env.OS = a.Snapshot.OSVersion
				env.Shell = a.Snapshot.Shell
				env.GitRepo = a.Snapshot.IsGitRepo
				env.Worktree = a.Snapshot.IsWorktree
			}
		}
		if env.Version == "" && e.Version != "" {
			env.Version = e.Version
		}
	}
	if reported != nil {
		env.Reported = reported.TotalCostUSD
		for model, u := range reported.ModelUsage {
			t := perModel[model]
			if t == nil {
				t = &tokenTotals{Model: model}
				perModel[model] = t
				modelOrder = append(modelOrder, model)
			}
			t.Input, t.Output, t.CacheRead, t.CacheCreation, t.Cost = u.Input, u.Output, u.CacheRead, u.CacheCreation, u.Cost
		}
	}
	for _, m := range modelOrder {
		t := perModel[m]
		env.Tokens = append(env.Tokens, *t)
		env.Total.Input += t.Input
		env.Total.Output += t.Output
		env.Total.CacheRead += t.CacheRead
		env.Total.CacheCreation += t.CacheCreation
		env.Total.Cost += t.Cost
		env.Total.Turns += t.Turns
	}
	env.Skills = skills.order
	env.Invoked = invoked.order
	env.MCP = mcp.order
	for name, n := range hooks {
		env.Hooks = append(env.Hooks, toolCount{name, n})
	}
	sort.Slice(env.Hooks, func(i, j int) bool {
		return env.Hooks[i].Count > env.Hooks[j].Count || (env.Hooks[i].Count == env.Hooks[j].Count && env.Hooks[i].Name < env.Hooks[j].Name)
	})
	for name, n := range tools {
		env.Tools = append(env.Tools, toolCount{name, n})
	}
	sort.Slice(env.Tools, func(i, j int) bool {
		return env.Tools[i].Count > env.Tools[j].Count || (env.Tools[i].Count == env.Tools[j].Count && env.Tools[i].Name < env.Tools[j].Name)
	})
	if len(env.Tools) > 12 {
		env.Tools = env.Tools[:12]
	}
	return env
}

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}

func fmtUSD(v float64) string {
	if v >= 1 {
		return "$" + strconv.FormatFloat(v, 'f', 2, 64)
	}
	return "$" + strconv.FormatFloat(v, 'f', 3, 64)
}

var presets = map[string][]string{
	"conversation": {"thinking", "turn_duration", "permission-mode", "mode", "meta", "queue-operation", "system", "pr-link", "frame-link", "attachment", "unknown"},
	"standard":     defaultHidden,
	"everything":   {},
}

func presetOf(hidden []string) string {
	key := func(l []string) string {
		c := append([]string(nil), l...)
		sort.Strings(c)
		return strings.Join(c, ",")
	}
	h := key(hidden)
	for _, name := range []string{"conversation", "standard", "everything"} {
		if key(presets[name]) == h {
			return name
		}
	}
	return ""
}

const (
	groupNone    = "none"
	groupProject = "project"
	groupHost    = "host"
)

func groupItems(items []*flatItem, mode string) []*sidebarGroup {
	if mode == groupNone || mode == "" {
		return nil
	}
	var out []*sidebarGroup
	index := map[string]*sidebarGroup{}
	for _, it := range items {
		var key, label string
		switch mode {
		case groupHost:
			key, label = it.File.Host, it.File.Host
			if key == "" {
				key, label = "~", "unknown host"
			}
		default:
			key, label = it.Folder, it.Folder
			if key == "" {
				key, label = "~", "unknown project"
			}
		}
		g := index[key]
		if g == nil {
			g = &sidebarGroup{Key: key, Label: label}
			index[key] = g
			out = append(out, g)
		}
		g.Items = append(g.Items, it)
	}
	return out
}

func childMarkers(flat []*flatItem, rel string) []Entry {
	var out []Entry
	for _, it := range flat {
		if it.File.Path != rel {
			continue
		}
		for _, a := range it.Agents {
			if isAgentPath(a.Path) || a.Facts == nil || a.Facts.Started == "" {
				continue
			}
			out = append(out, Entry{
				Kind: KindChildSession, TS: a.Facts.Started, Agent: a.Agent,
				AgentID: a.AgentID, Text: a.Title, SubAgentPath: a.Path,
			})
		}
	}
	return out
}

func mergeMarkers(entries []Entry, markers []Entry) []Entry {
	if len(markers) == 0 {
		return entries
	}
	out := make([]Entry, 0, len(entries)+len(markers))
	mi := 0
	sort.SliceStable(markers, func(i, j int) bool { return markers[i].TS < markers[j].TS })
	for _, e := range entries {
		for mi < len(markers) && e.TS != "" && markers[mi].TS <= e.TS {
			out = append(out, markers[mi])
			mi++
		}
		out = append(out, e)
	}
	out = append(out, markers[mi:]...)
	return out
}
