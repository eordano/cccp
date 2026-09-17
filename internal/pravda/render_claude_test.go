package pravda

import (
	"strings"
	"testing"
)

func entriesOf(t *testing.T, jsonl string) []Entry {
	t.Helper()
	events, err := ParseJSONL(strings.NewReader(jsonl))
	if err != nil {
		t.Fatal(err)
	}
	return Normalize(events, nil).Entries
}

func findEntry(entries []Entry, pred func(*Entry) bool) *Entry {
	for i := range entries {
		if pred(&entries[i]) {
			return &entries[i]
		}
	}
	return nil
}

func toolEntry(entries []Entry, name string) *Entry {
	return findEntry(entries, func(e *Entry) bool { return e.Kind == KindTool && e.Name == name })
}

func TestCommandMessageFirstIsSlashCommand(t *testing.T) {
	es := entriesOf(t, `{"type":"user","message":{"role":"user","content":"<command-message>reduce-tokens</command-message>\n<command-name>/reduce-tokens</command-name>\n<command-args>docs/</command-args>"}}`)
	if len(es) != 1 || es[0].Kind != KindCommand {
		t.Fatalf("got %+v, want one command entry", es)
	}
	if es[0].CommandName != "/reduce-tokens" || es[0].CommandArgs != "docs/" || es[0].CommandMessage != "reduce-tokens" {
		t.Errorf("command fields = %q %q %q", es[0].CommandName, es[0].CommandArgs, es[0].CommandMessage)
	}
}

func TestUserListContent(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	es := sess.Entries

	img := findEntry(es, func(e *Entry) bool { return e.Kind == KindUser && len(e.Images) > 0 })
	if img == nil {
		t.Fatal("pasted-image prompt did not become a user entry with images")
	}
	if !strings.HasPrefix(img.Text, "[Image #1]") {
		t.Errorf("image prompt text = %q", img.Text)
	}
	html := string(renderEntryContent(img))
	if !strings.Contains(html, `<img`) || !strings.Contains(html, "data:image/png;base64,") {
		t.Errorf("image prompt rendered without inline <img>: %s", html)
	}

	meta := findEntry(es, func(e *Entry) bool {
		return e.Kind == KindMeta && strings.HasPrefix(e.Text, "Base directory for this skill")
	})
	if meta == nil {
		t.Error("isMeta list text did not become a meta entry")
	}

	var interrupts []string
	for _, e := range es {
		if e.Kind == KindInterrupt {
			interrupts = append(interrupts, e.Text)
		}
	}
	if len(interrupts) != 1 || interrupts[0] != "[Request interrupted by user for tool use]" {
		t.Errorf("interrupts = %q, want only the genuine one", interrupts)
	}
}

func TestOversizedImageFallsBackToMarker(t *testing.T) {
	big := ImageBlock{MediaType: "image/png", Data: strings.Repeat("A", maxInlineImageBase64+1)}
	if big.DataURL() != "" {
		t.Error("oversized image should not inline")
	}
	e := &Entry{Kind: KindUser, Text: "hi", Images: []ImageBlock{big}}
	if html := string(renderEntryContent(e)); !strings.Contains(html, "[image image/png]") {
		t.Errorf("no marker for oversized image: %s", html)
	}
}

func TestAgentWithoutAgentRefRenders(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	e := toolEntry(sess.Entries, "Agent")
	if e == nil {
		t.Fatal("no Agent tool entry")
	}
	if e.AgentRef != nil {
		t.Fatal("fixture Agent should have failed without an agentId")
	}
	html := string(renderEntryContent(e))
	if strings.Contains(html, "render error") {
		t.Fatalf("render error: %s", html)
	}
	if !strings.Contains(html, "Failing spawn") || !strings.Contains(html, "agent-failed") {
		t.Errorf("failed spawn not shown: %s", html)
	}
}

func TestReadImageResult(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	e := toolEntry(sess.Entries, "Read")
	if e == nil {
		t.Fatal("no Read tool entry")
	}
	info := e.ImageResult()
	if info == nil || info.W != 1280 || info.H != 800 || info.Size != 7890 {
		t.Fatalf("image info = %+v", info)
	}
	html := string(renderEntryContent(e))
	if !strings.Contains(html, "1280×800") || !strings.Contains(html, "<img") {
		t.Errorf("read image not rendered: %s", html)
	}
	if strings.Contains(html, `"type":"image"`) || strings.Contains(html, `"source"`) {
		t.Errorf("base64 JSON leaked into read render")
	}
}

func TestAssistantFallbackItem(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	e := findEntry(sess.Entries, func(e *Entry) bool { return e.Kind == KindSystem && e.Subtype == "model_fallback" })
	if e == nil {
		t.Fatal("fallback content item produced no entry")
	}
	if e.Mode != "claude-fable-5-1" || e.ToModel != "claude-opus-4-8" {
		t.Errorf("fallback models = %q -> %q", e.Mode, e.ToModel)
	}
	if html := string(renderEntryContent(e)); !strings.Contains(html, "claude-opus-4-8") {
		t.Errorf("fallback render: %s", html)
	}
}

func TestSystemSubtypes(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	bySub := func(sub string) *Entry {
		return findEntry(sess.Entries, func(e *Entry) bool { return e.Kind == KindSystem && e.Subtype == sub })
	}
	fb := bySub("model_refusal_fallback")
	if fb == nil {
		t.Fatal("no model_refusal_fallback entry")
	}
	if fb.Mode != "claude-fable-5-1" || fb.ToModel != "claude-opus-4-8" || fb.Status != "cyber" || fb.Direction != "retry" || fb.Retracted != 2 || fb.Text == "" {
		t.Errorf("refusal fallback fields not lifted: %+v", fb)
	}
	if html := string(renderEntryContent(fb)); !strings.Contains(html, "2 messages retracted") {
		t.Errorf("refusal fallback render: %s", html)
	}
	if fb.OpClass() != "op-warn" || fb.Icon() != "⛔" {
		t.Errorf("refusal fallback class/icon = %s %s", fb.OpClass(), fb.Icon())
	}

	bs := bySub("bridge_status")
	if bs == nil || bs.URL != "https://claude.ai/code/session_demo" {
		t.Fatalf("bridge_status = %+v", bs)
	}
	if html := string(renderEntryContent(bs)); !strings.Contains(html, `href="https://claude.ai/code/session_demo"`) {
		t.Errorf("bridge url not linked: %s", html)
	}

	info := bySub("informational")
	if info == nil || info.Icon() != "ℹ️" || info.Label() != "Info" {
		t.Fatalf("informational = %+v", info)
	}
	if html := string(renderEntryContent(info)); !strings.Contains(html, "Unknown command: /safe-mode") {
		t.Errorf("informational render: %s", html)
	}

	goal := bySub("goal")
	if goal == nil || goal.Text != "Ship the viewer" || goal.Status != "active" {
		t.Errorf("goal = %+v", goal)
	}

	td := findEntry(sess.Entries, func(e *Entry) bool { return e.Kind == KindTurnDuration && e.Level == "error" })
	if td == nil || td.Text != "server_overloaded" {
		t.Fatalf("turn_duration error not lifted: %+v", td)
	}
	if html := string(renderEntryContent(td)); !strings.Contains(html, "server_overloaded") {
		t.Errorf("turn error not rendered: %s", html)
	}
}

func TestModelChangeTransitions(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	var texts []string
	for _, e := range sess.Entries {
		if e.Kind == KindMode && e.Subtype == "model-change" {
			texts = append(texts, e.Text)
		}
	}
	want := []string{"model → gpt-5 (effort high)", "model → gpt-5 (effort medium)", "model → Fable 5.1"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("model transitions = %q, want %q", texts, want)
	}
	if e := findEntry(sess.Entries, func(e *Entry) bool { return e.Kind == KindAttachment && e.Subtype == "model" }); e != nil {
		t.Error("raw attachment:model should not surface")
	}
}

func TestDroppedAttachments(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	for _, sub := range []string{"session_context", "silent_turn_reminder", "environment", "batching_reminder_sent", "bash_output_audience_note", "model", "date"} {
		if e := findEntry(sess.Entries, func(e *Entry) bool { return e.Kind == KindAttachment && e.Subtype == sub }); e != nil {
			t.Errorf("attachment:%s should be dropped", sub)
		}
	}
	for _, e := range sess.Entries {
		if html := string(renderEntryContent(&e)); strings.Contains(html, "someone@example.com") {
			t.Errorf("session_context email leaked into %s/%s", e.Kind, e.Subtype)
		}
	}
}

func TestDateOnlyOnChange(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	var dates []string
	for _, e := range sess.Entries {
		if e.Kind == KindAttachment && e.Subtype == "date_change" {
			dates = append(dates, e.Text)
		}
	}
	if strings.Join(dates, ",") != "2026-08-31,2026-08-30,2026-08-31" {
		t.Errorf("date changes = %q, want only transitions (the repeated 2026-08-30 dropped)", dates)
	}
}

func TestAttachmentRenderers(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	cases := map[string][]string{
		"remote_session_change":  {`href="https://claude.ai/code/session_demo"`, "trailers"},
		"instructions":           {"2 instruction files", "/repo/CLAUDE.md", "AutoMem"},
		"prompt_snapshot":        {"3 sections", "section 1"},
		"deferred_tools_record":  {"2 tool schemas: Monitor, SendUserFile"},
		"read_truncation_notice": {"Read truncated", "PARTIAL view", "toolu_r1"},
		"auto_mode_exit":         {"exited auto mode", "bash-first"},
		"dynamic_skill":          {"vendor/x:reduce-tokens", "vendor/x/.claude/skills"},
		"thinking_stripped":      {"scope all"},
		"structured_output":      {"❌ failed", "2 problems", "test B flaky"},
	}
	for sub, wants := range cases {
		e := findEntry(sess.Entries, func(e *Entry) bool { return e.Kind == KindAttachment && e.Subtype == sub })
		if e == nil {
			t.Errorf("attachment:%s: no entry", sub)
			continue
		}
		html := string(renderEntryContent(e))
		if strings.Contains(html, "No renderer") || strings.Contains(html, `<summary class="content-preview">raw</summary>`) {
			t.Errorf("attachment:%s fell through to the generic branch", sub)
		}
		for _, w := range wants {
			if !strings.Contains(html, w) {
				t.Errorf("attachment:%s: missing %q in %s", sub, w, html)
			}
		}
	}
}

func TestToolRenderers(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	cases := map[string]struct {
		tmpl  string
		wants []string
	}{
		"StructuredOutput":                {"tool-StructuredOutput", []string{"reason, refuted", "Structured output provided successfully"}},
		"AskUserQuestion":                 {"tool-AskUserQuestion", []string{"1 question · answered", "Target: memory dir", "✅ memory dir", "⭕ docs/"}},
		"Skill":                           {"tool-Skill", []string{"/reduce-tokens docs/", "launched reduce-tokens"}},
		"Artifact":                        {"tool-Artifact", []string{`href="https://claude.ai/code/artifact/demo"`, "Thirty-Two Bytes", "v3"}},
		"SendUserFile":                    {"tool-SendUserFile", []string{"Guest desktop", "shot.png", "1 delivered"}},
		"Workflow":                        {"tool-Workflow", []string{"demo", "async_launched", "wf_174648cc-f8f", "Script · 2 lines"}},
		"Monitor":                         {"tool-Monitor", []string{"Watch the build log", "Monitor started"}},
		"TaskStop":                        {"tool-TaskStop", []string{"task b8fb353v5", "Successfully stopped task"}},
		"TaskOutput":                      {"tool-TaskOutput", []string{"task bknty3cok", "hermes rc=0"}},
		"SendMessage":                     {"tool-SendMessage", []string{"→ sibling-a4: ping the sibling", "Message queued"}},
		"ListAgents":                      {"tool-ListAgents", []string{"list agents", "Peer sessions"}},
		"PushNotification":                {"tool-PushNotification", []string{"Sign-in blocked"}},
		"EnterPlanMode":                   {"tool-EnterPlanMode", []string{"enter plan mode", "Entered plan mode."}},
		"mcp__claude-in-chrome__computer": {"tool-mcp-chrome", []string{"left_click", "(100, 218)", "Clicked at (100, 218)", "<img"}},
		"mcp__claude-in-chrome__navigate": {"tool-mcp-chrome", []string{"https://example.com/", "Navigation failed"}},
	}
	for name, tc := range cases {
		e := toolEntry(sess.Entries, name)
		if e == nil {
			t.Errorf("%s: no tool entry", name)
			continue
		}
		if got := lookupTemplate(e); got != tc.tmpl {
			t.Errorf("%s: template = %s, want %s", name, got, tc.tmpl)
		}
		html := string(renderEntryContent(e))
		if strings.Contains(html, "render error") {
			t.Errorf("%s: %s", name, html)
		}
		for _, w := range tc.wants {
			if !strings.Contains(html, w) {
				t.Errorf("%s: missing %q in %s", name, w, html)
			}
		}
	}
	if e := toolEntry(sess.Entries, "mcp__claude-in-chrome__computer"); e != nil && e.Label() != "computer" {
		t.Errorf("mcp label = %q", e.Label())
	}
}
