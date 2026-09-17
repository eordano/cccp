package pravda

import (
	"os"
	"strings"
	"testing"
)

func normalizeFixture(t *testing.T, name string) *Session {
	t.Helper()
	f, err := os.Open("fixtures/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	events, err := ParseJSONL(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return Normalize(events, nil)
}

func lookupTemplate(e *Entry) string {
	for _, c := range e.TemplateNames() {
		if tmpl.Lookup(c) != nil {
			return c
		}
	}
	return "event-unknown"
}

func TestNewKindsAllReachARenderer(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	if len(sess.Entries) == 0 {
		t.Fatal("no entries")
	}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.Kind == KindUnknown {
			t.Errorf("entry %d has unknown kind: %s", e.Index, e.Data)
			continue
		}
		if got := lookupTemplate(e); got == "event-unknown" {
			t.Errorf("entry %d (%s/%s) has no renderer", e.Index, e.Kind, e.Subtype)
		}
		if html := string(renderEntryContent(e)); strings.Contains(html, "<!-- render error:") {
			t.Errorf("entry %d (%s/%s): %s", e.Index, e.Kind, e.Subtype, html)
		}
	}
}

func TestNewKindsFields(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	byKind := map[string]*Entry{}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		key := e.Kind
		if e.Subtype != "" {
			key += ":" + e.Subtype
		}
		if _, ok := byKind[key]; !ok {
			byKind[key] = e
		}
	}

	cases := []struct {
		key   string
		check func(*Entry) string
	}{
		{"bash-input", func(e *Entry) string {
			if e.Text != "echo hi" {
				return "want command text, got " + e.Text
			}
			return ""
		}},
		{"bash-output", func(e *Entry) string {
			if e.Text != "hi" || e.Stderr != "warn: careful" {
				return "stdout/stderr not split: " + e.Text + " | " + e.Stderr
			}
			return ""
		}},
		{"attachment:total_tokens_reminder", func(e *Entry) string {
			if e.TokensLeft != 15000000 {
				return "token count not parsed"
			}
			return ""
		}},
		{"system:compact_boundary", func(e *Entry) string {
			if e.Trigger != "manual" || e.PreTokens != 927013 || e.PostTokens != 13359 {
				return "compact metadata not lifted"
			}
			return ""
		}},
		{"system:model_refusal_no_fallback", func(e *Entry) string {
			if e.Status != "reasoning_extraction" || e.Text == "" {
				return "refusal fields not lifted"
			}
			return ""
		}},
		{"pr-link", func(e *Entry) string {
			if e.Number != 2 || e.Repo != "owner/repo" {
				return "pr fields not lifted"
			}
			return ""
		}},
		{"frame-link", func(e *Entry) string {
			if e.Text != "A Published Page" || e.URL == "" {
				return "frame fields not lifted"
			}
			return ""
		}},
		{"command", func(e *Entry) string {
			if e.CommandName != "/artifacts" {
				return "command name not parsed: " + e.CommandName
			}
			return ""
		}},
	}
	for _, tc := range cases {
		e, ok := byKind[tc.key]
		if !ok {
			t.Errorf("%s: no entry produced", tc.key)
			continue
		}
		if msg := tc.check(e); msg != "" {
			t.Errorf("%s: %s", tc.key, msg)
		}
	}

	for _, dropped := range []string{"bridge-session", "ai-title", "agent-name", "file-history-delta"} {
		if _, ok := byKind[dropped]; ok {
			t.Errorf("%s should not produce a timeline entry", dropped)
		}
	}
}

func TestAITitleWins(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	if got := extractTitleFromEvents(sess.Events); got != "Generated session title" {
		t.Errorf("title = %q, want the ai-title", got)
	}
}

func TestMCPToolLabelIsShortened(t *testing.T) {
	sess := normalizeFixture(t, "new-kinds.jsonl")
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.Kind == KindTool && strings.HasPrefix(e.Name, "mcp__") {
			if e.Label() != "do_thing" {
				t.Errorf("mcp label = %q, want do_thing", e.Label())
			}
			return
		}
	}
	t.Fatal("no mcp tool entry in fixture")
}

func TestRepeatedModeEventsCollapse(t *testing.T) {
	events, err := ParseJSONL(strings.NewReader(
		`{"type":"mode","mode":"normal"}` + "\n" +
			`{"type":"mode","mode":"normal"}` + "\n" +
			`{"type":"mode","mode":"plan"}` + "\n" +
			`{"type":"permission-mode","permissionMode":"default"}` + "\n" +
			`{"type":"permission-mode","permissionMode":"default"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	sess := Normalize(events, nil)
	modes, perms := 0, 0
	for _, e := range sess.Entries {
		switch e.Kind {
		case KindMode:
			modes++
		case KindPermissionMode:
			perms++
		}
	}
	if modes != 2 {
		t.Errorf("mode entries = %d, want 2", modes)
	}
	if perms != 1 {
		t.Errorf("permission-mode entries = %d, want 1", perms)
	}
}
