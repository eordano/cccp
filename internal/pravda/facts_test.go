package pravda

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubagentPaths(t *testing.T) {
	cases := map[string][2]string{
		"projects/p/s1/subagents/agent-abc.jsonl":                  {"projects/p/s1.jsonl", "abc"},
		"projects/p/s1/subagents/workflows/wf_x-1/agent-abc.jsonl": {"projects/p/s1.jsonl", "wf_x-1 › abc"},
		"projects/p/s1/subagents/workflows/wf_x-1/journal.jsonl":   {"projects/p/s1.jsonl", "wf_x-1 › journal"},
		"projects/-home-alice/s2/subagents/agent-1.jsonl":          {"projects/-home-alice/s2.jsonl", "1"},
	}
	for in, want := range cases {
		if !isAgentPath(in) {
			t.Errorf("%s not recognised as sub-agent", in)
		}
		if got := parentSessionOf(in); got != want[0] {
			t.Errorf("parent(%s) = %s, want %s", in, got, want[0])
		}
		if got := subagentLabel(in); got != want[1] {
			t.Errorf("label(%s) = %q, want %q", in, got, want[1])
		}
	}
	for _, in := range []string{"projects/p/s1.jsonl", "agent/codex/sessions/2026/09/08/r.jsonl", "agents/reviewer.jsonl"} {
		if isAgentPath(in) {
			t.Errorf("%s wrongly recognised as sub-agent", in)
		}
	}
}

func TestPrettyCWD(t *testing.T) {
	cases := map[string]string{
		"/home/alice/projects/app":   "~alice/projects/app",
		"/home/bob/workspaces/a/b/c": "~bob/…/b/c",
		"/srv/repo":                  "/srv/repo",
		"/Users/EXAMPLE/src/x":       "~EXAMPLE/src/x",
		"":                           "",
	}
	for in, want := range cases {
		if got := prettyCWD(in); got != want {
			t.Errorf("prettyCWD(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUserPromptTitleSkipsMetaAndToolResults(t *testing.T) {
	mk := func(content string, meta bool) *RawEvent {
		return &RawEvent{Type: "user", IsMeta: meta, Message: &Message{Role: "user", Content: json.RawMessage(content)}}
	}
	if got := userPromptTitle(mk(`[{"type":"tool_result","tool_use_id":"x","content":"total 140"}]`, false)); got != "" {
		t.Errorf("tool_result array must not title a session, got %q", got)
	}
	if got := userPromptTitle(mk(`"# AGENTS.md instructions for /srv/repo\n..."`, false)); got != "" {
		t.Errorf("AGENTS.md preamble must not title a session, got %q", got)
	}
	if got := userPromptTitle(mk(`"real prompt"`, true)); got != "" {
		t.Errorf("meta lines must not title a session, got %q", got)
	}
	if got := userPromptTitle(mk(`[{"type":"text","text":"fix the build"},{"type":"tool_result","tool_use_id":"x","content":"y"}]`, false)); got != "fix the build" {
		t.Errorf("text block in array should title, got %q", got)
	}
}

func TestToolPreview(t *testing.T) {
	e := &Entry{Kind: KindTool, Name: "terminal", Input: json.RawMessage(`{"command":"echo hi\nsecond","timeout":5}`)}
	_ = json.Unmarshal(e.Input, &e.InputMap)
	if got := toolPreview(e); got != "echo hi" {
		t.Errorf("preview = %q", got)
	}
	e2 := &Entry{Kind: KindTool, Name: "exec", Input: json.RawMessage(`"const x = 1;"`)}
	if got := toolPreview(e2); got != "const x = 1;" {
		t.Errorf("string input preview = %q", got)
	}
	if got := toolPreview(&Entry{Kind: KindTool, Name: "noop", Input: json.RawMessage(`{}`)}); got != "" {
		t.Errorf("empty input must have no preview, got %q", got)
	}
}

func TestSidebarNestsCodexChildrenAndWorkflows(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	parent := `{"timestamp":"2026-09-08T00:00:00Z","type":"session_meta","payload":{"id":"P1","cwd":"/srv/repo","cli_version":"0.153.4","git":{"branch":"main"}}}
{"timestamp":"2026-09-08T00:00:01Z","type":"response_item","payload":{"type":"message","id":"m1","role":"user","content":[{"type":"input_text","text":"do the thing"}]}}
`
	child := `{"timestamp":"2026-09-08T00:00:02Z","type":"session_meta","payload":{"id":"C1","parent_thread_id":"P1","thread_source":"guardian_review","cwd":"/srv/repo"}}
{"timestamp":"2026-09-08T00:00:03Z","type":"response_item","payload":{"type":"message","id":"m2","role":"user","content":[{"type":"input_text","text":"The following is the Codex agent history"}]}}
`
	write("agent/codex/sessions/2026/09/08/rollout-a-P1.jsonl", parent)
	write("agent/codex/sessions/2026/09/08/rollout-b-C1.jsonl", child)
	claude := `{"type":"user","uuid":"u1","timestamp":"2026-09-08T00:00:00Z","cwd":"/home/alice/projects/x","sessionId":"s1","version":"2.1.0","gitBranch":"dev","message":{"role":"user","content":"hello there"}}
`
	write("projects/-home-alice-projects-x/s1.jsonl", claude)
	write("projects/-home-alice-projects-x/s1/subagents/workflows/wf_1/agent-a.jsonl", `{"type":"user","uuid":"u2","timestamp":"2026-09-08T00:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}}`+"\n")
	write("projects/-home-alice-projects-x/s1/subagents/workflows/wf_1/journal.jsonl", `{"type":"user","uuid":"u3","timestamp":"2026-09-08T00:00:01Z","message":{"role":"user","content":"journal"}}`+"\n")

	srv := NewWatchServer(NewFSStore(dir))
	flat, err := srv.buildSidebar(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) != 2 {
		for _, f := range flat {
			t.Logf("top: %s", f.File.Path)
		}
		t.Fatalf("want 2 top-level sessions, got %d", len(flat))
	}
	byPath := map[string]*flatItem{}
	for _, f := range flat {
		byPath[f.File.Path] = f
	}
	cx := byPath["agent/codex/sessions/2026/09/08/rollout-a-P1.jsonl"]
	if cx == nil || len(cx.Agents) != 1 || cx.Agents[0].AgentID != "guardian_review" || cx.Agents[0].ParentPath != cx.File.Path {
		t.Fatalf("codex child not nested under parent: %+v", cx)
	}
	if cx.File.Agent != "codex" || cx.Folder != "/srv/repo" || cx.File.Title != "do the thing" {
		t.Errorf("codex facts: agent=%s folder=%s title=%q", cx.File.Agent, cx.Folder, cx.File.Title)
	}
	cl := byPath["projects/-home-alice-projects-x/s1.jsonl"]
	if cl == nil || len(cl.Agents) != 2 {
		t.Fatalf("workflow files not nested: %+v", cl)
	}
	if cl.Folder != "~alice/projects/x" || cl.File.Agent != "claude" || cl.File.Facts.GitBranch != "dev" {
		t.Errorf("claude facts: folder=%s agent=%s facts=%+v", cl.Folder, cl.File.Agent, cl.File.Facts)
	}
	counts := countAgents(flat)
	if len(counts) != 2 {
		t.Errorf("agent counts = %+v", counts)
	}
}

func TestTimelineWindow(t *testing.T) {
	sess := &Session{}
	for i := 0; i < 1000; i++ {
		k := KindMessage
		if i%2 == 0 {
			k = KindThinking
		}
		sess.Entries = append(sess.Entries, Entry{Kind: k, Index: i + 1, Text: "x"})
	}
	td := newTimeline(sess, []string{"thinking"})
	td.Path = "p/s.jsonl"
	vis := td.Visible()
	if td.Total != 500 || td.Shown != 400 || td.Skipped != 100 || len(vis) != 400 {
		t.Fatalf("window: total=%d shown=%d skipped=%d len=%d", td.Total, td.Shown, td.Skipped, len(vis))
	}
	if vis[0].Kind != KindMessage || vis[len(vis)-1].Index != 1000 {
		t.Fatalf("window must end at the newest visible entry, got first=%s last=%d", vis[0].Kind, vis[len(vis)-1].Index)
	}
	if td.EarlierURL() == "" || td.AllURL() != "/session/p/s.jsonl?limit=0" {
		t.Fatalf("urls: %q %q", td.EarlierURL(), td.AllURL())
	}
	td.Limit = 0
	if got := td.Visible(); len(got) != 500 || td.Skipped != 0 || td.AllURL() != "" {
		t.Fatalf("limit=0 must show everything, got %d", len(got))
	}
}

func TestBuildEnvAndPresets(t *testing.T) {
	lines := `{"type":"assistant","uuid":"a1","timestamp":"2026-09-08T00:00:01Z","version":"2.1.0","effort":"high","message":{"role":"assistant","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40},"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"interview"}},{"type":"tool_use","id":"t2","name":"mcp__chrome__navigate","input":{"url":"x"}}]}}
{"type":"usage","timestamp":"2026-09-08T00:00:02Z","model":"gpt-5","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"costUSD":0.5}
{"type":"model-change","timestamp":"2026-09-08T00:00:03Z","model":"gpt-5","effort":"low"}
{"type":"attachment","uuid":"at1","timestamp":"2026-09-08T00:00:04Z","attachment":{"type":"skill_listing","names":["a","b"]}}
{"type":"attachment","uuid":"at2","timestamp":"2026-09-08T00:00:05Z","attachment":{"type":"mcp_instructions_delta","addedNames":["chrome","figma"]}}
{"type":"attachment","uuid":"at3","timestamp":"2026-09-08T00:00:06Z","attachment":{"type":"hook_success","hookName":"bash-guardrails"}}
{"type":"attachment","uuid":"at4","timestamp":"2026-09-08T00:00:07Z","attachment":{"type":"environment","snapshot":{"platform":"linux","shell":"fish","isGitRepo":true}}}
{"type":"cost-state","totalCostUSD":1.25,"modelUsage":{"claude-opus-5":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":30,"cacheCreationInputTokens":40,"costUSD":0.75}}}
`
	events, err := ParseJSONL(strings.NewReader(lines))
	if err != nil {
		t.Fatal(err)
	}
	sess := Normalize(events, nil)
	env := buildEnv(sess)
	if env.Empty() {
		t.Fatal("env must not be empty")
	}
	if len(env.Models) != 2 || env.Models[0].Model != "claude-opus-5" || env.Models[1].Model != "gpt-5" || env.Models[1].Effort != "low" {
		t.Fatalf("models = %+v", env.Models)
	}
	if env.Reported != 1.25 || env.Cost() != 1.25 || len(env.Tokens) != 2 || env.Total.Input != 11 {
		t.Fatalf("tokens/cost = %+v total=%+v reported=%v", env.Tokens, env.Total, env.Reported)
	}
	if len(env.Skills) != 2 || len(env.Invoked) != 1 || env.Invoked[0] != "interview" {
		t.Fatalf("skills = %v invoked = %v", env.Skills, env.Invoked)
	}
	if len(env.MCP) != 2 || env.MCP[0] != "chrome" || env.Platform != "linux" || env.Shell != "fish" || !env.GitRepo || env.Version != "2.1.0" {
		t.Fatalf("mcp=%v platform=%s shell=%s git=%v version=%s", env.MCP, env.Platform, env.Shell, env.GitRepo, env.Version)
	}
	if len(env.Hooks) != 1 || env.Hooks[0].Name != "bash-guardrails" || env.ToolUses != 2 {
		t.Fatalf("hooks=%v tools=%d", env.Hooks, env.ToolUses)
	}
	if presetOf(defaultHidden) != "standard" || presetOf(nil) != "everything" || presetOf([]string{"thinking"}) != "" {
		t.Fatal("presetOf")
	}
	if fmtTokens(1500) != "1.5k" || fmtTokens(2_000_000) != "2.0M" || fmtUSD(0.5) != "$0.500" || fmtUSD(12.3) != "$12.30" {
		t.Fatal("formatting")
	}
}

func TestGroupItemsAndMarkers(t *testing.T) {
	mk := func(path, folder, host, mod string) *flatItem {
		return &flatItem{File: &fileInfo{Path: path, Folder: folder, Host: host, Modified: mod, Agent: "claude"}, Folder: folder}
	}
	items := []*flatItem{mk("a", "/p1", "alpha", "3"), mk("b", "/p2", "beta", "2"), mk("c", "/p1", "alpha", "1"), mk("d", "", "", "0")}
	g := groupItems(items, groupProject)
	if len(g) != 3 || g[0].Key != "/p1" || len(g[0].Items) != 2 || g[2].Label != "unknown project" {
		t.Fatalf("project groups = %+v", g)
	}
	h := groupItems(items, groupHost)
	if len(h) != 3 || h[0].Key != "alpha" || h[1].Key != "beta" || h[2].Label != "unknown host" {
		t.Fatalf("host groups = %+v", h)
	}
	if groupItems(items, groupNone) != nil {
		t.Fatal("none must not group")
	}
	parent := mk("agent/codex/sessions/p.jsonl", "/p1", "", "5")
	parent.Agents = []*fileInfo{
		{Path: "agent/codex/sessions/c.jsonl", Agent: "codex", AgentID: "guardian_review", Title: "review", Facts: &sessionFacts{Started: "2026-09-08T00:00:02Z"}},
		{Path: "projects/p/s/subagents/agent-x.jsonl", Agent: "claude", Facts: &sessionFacts{Started: "2026-09-08T00:00:02Z"}},
	}
	markers := childMarkers([]*flatItem{parent}, "agent/codex/sessions/p.jsonl")
	if len(markers) != 1 || markers[0].Kind != KindChildSession || markers[0].SubAgentPath != "agent/codex/sessions/c.jsonl" {
		t.Fatalf("markers = %+v", markers)
	}
	entries := []Entry{{Kind: KindUser, TS: "2026-09-08T00:00:01Z", Index: 1}, {Kind: KindMessage, TS: "2026-09-08T00:00:03Z", Index: 2}}
	merged := mergeMarkers(entries, markers)
	if len(merged) != 3 || merged[1].Kind != KindChildSession {
		t.Fatalf("merged order wrong: %v %v %v", merged[0].Kind, merged[1].Kind, merged[2].Kind)
	}
}

func TestFolderForSlugFallback(t *testing.T) {
	if got := folderFor("projects/-srv-repo/s.jsonl", nil); got != "/srv/repo" {
		t.Fatalf("slug fallback = %q", got)
	}
	if got := folderFor("projects/-home-alice-x/s.jsonl", &sessionFacts{CWD: "/home/alice/x"}); got != "~alice/x" {
		t.Fatalf("cwd wins = %q", got)
	}
}
