package pravda

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestHermesConverterIsIdentityForJSONLines(t *testing.T) {
	raw, err := os.ReadFile("fixtures/hermes-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	st := withConverters(memStore{lines: map[string]string{
		"agent/hermes/transcripts/s.jsonl": string(raw) + "\n\nnot json\n",
	}})
	rc, err := st.OpenSession(context.Background(), "agent/hermes/transcripts/s.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != string(raw) {
		t.Fatalf("converted transcript differs from source\n got: %q\nwant: %q", got, raw)
	}
}

func hermesUsageEvents(t *testing.T, sess *Session, want int) []*RawEvent {
	t.Helper()
	var out []*RawEvent
	for _, e := range sess.Events {
		if e.Type == "usage" {
			out = append(out, e)
		}
	}
	if len(out) != want {
		t.Fatalf("usage events = %d, want %d", len(out), want)
	}
	for i := range sess.Entries {
		if src := sess.Entries[i].RawSource; src != nil && src.Type == "usage" {
			t.Errorf("entry %d rendered from a usage line", i)
		}
	}
	return out
}

func TestHermesTranscriptNormalizes(t *testing.T) {
	sess := normalizeFixture(t, "hermes-session.jsonl")
	kinds := make([]string, 0, len(sess.Entries))
	for i := range sess.Entries {
		e := &sess.Entries[i]
		kinds = append(kinds, e.Kind)
		if e.Kind == KindUnknown {
			t.Errorf("entry %d unknown kind: %s", e.Index, e.Data)
		}
		if got := lookupTemplate(e); got == "event-unknown" {
			t.Errorf("entry %d (%s) has no renderer", e.Index, e.Kind)
		}
		if html := string(renderEntryContent(e)); strings.Contains(html, "<!-- render error:") {
			t.Errorf("entry %d (%s): %s", e.Index, e.Kind, html)
		}
	}
	want := []string{KindMode, KindUser, KindTool, KindMeta, KindThinking, KindMessage}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if mode := sess.Entries[0]; mode.Subtype != "model-change" || mode.Mode != "openai/gpt-5.5" {
		t.Errorf("model-change entry = %+v", mode)
	}
	user := sess.Entries[1]
	if !strings.HasPrefix(user.Text, "List the go files") || user.CWD != "/home/dev/todo" {
		t.Errorf("user entry = %+v", user)
	}
	tool := sess.Entries[2]
	if tool.Name != "Bash" || tool.ID != "call_9f3b1c" || !tool.HasResult || tool.IsError || lookupTemplate(&tool) != "tool-Bash" {
		t.Errorf("tool entry = name %q id %q result %v err %v template %q", tool.Name, tool.ID, tool.HasResult, tool.IsError, lookupTemplate(&tool))
	}
	if !strings.Contains(string(tool.ResultContent), "store.go") || tool.InputMap["command"] == nil || string(tool.InputMap["hermes_tool"]) != `"terminal"` {
		t.Errorf("tool result/input not carried: %s / %v", tool.ResultContent, tool.InputMap)
	}
	if meta := sess.Entries[3]; !strings.HasPrefix(meta.Text, "[System]") {
		t.Errorf("meta entry = %+v", meta)
	}
	if !strings.Contains(sess.Entries[5].Text, "main()") {
		t.Errorf("assistant text = %q", sess.Entries[5].Text)
	}
	if title := extractTitleFromEvents(sess.Events); !strings.HasPrefix(title, "List the go files") {
		t.Errorf("title = %q", title)
	}
	usage := hermesUsageEvents(t, sess, 2)
	if usage[0].Model != "openai/gpt-5.5" || usage[0].Usage == nil || usage[0].Usage.Input != 2048 || usage[0].Usage.CacheRead != 1024 {
		t.Errorf("usage event = model %q usage %+v", usage[0].Model, usage[0].Usage)
	}
}

func TestHermesBackfillTranscriptNormalizes(t *testing.T) {
	sess := normalizeFixture(t, "hermes-backfill.jsonl")
	kinds := make([]string, 0, len(sess.Entries))
	for i := range sess.Entries {
		e := &sess.Entries[i]
		kinds = append(kinds, e.Kind)
		if e.Kind == KindUnknown || lookupTemplate(e) == "event-unknown" {
			t.Errorf("entry %d (%s) has no renderer", e.Index, e.Kind)
		}
		if html := string(renderEntryContent(e)); strings.Contains(html, "<!-- render error:") {
			t.Errorf("entry %d (%s): %s", e.Index, e.Kind, html)
		}
	}
	if want := []string{KindMode, KindUser, KindTool, KindMessage}; strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	tool := sess.Entries[2]
	if tool.Name != "Bash" || tool.ID != "call_E0skFxKdcckqQXm1Fklw2wi5" || !tool.HasResult || tool.IsError {
		t.Errorf("tool entry = name %q id %q result %v err %v", tool.Name, tool.ID, tool.HasResult, tool.IsError)
	}
	if !strings.Contains(string(tool.ResultContent), "cccp-e2e-ok") || tool.InputMap["command"] == nil || string(tool.InputMap["hermes_tool"]) != `"terminal"` {
		t.Errorf("tool result/input not carried: %s / %v", tool.ResultContent, tool.InputMap)
	}
	if title := extractTitleFromEvents(sess.Events); !strings.HasPrefix(title, "Run the shell command") {
		t.Errorf("title = %q", title)
	}
	if sess.Entries[1].CWD != "/home/dev/scratch" || sess.Entries[1].TS == "" || sess.Entries[1].TS >= sess.Entries[3].TS {
		t.Errorf("cwd/timestamps not carried: %+v", sess.Entries[1])
	}
	usage := hermesUsageEvents(t, sess, 2)
	if usage[0].Usage == nil || usage[0].Usage.Input != 10960 || usage[0].CostUSD == 0 || usage[1].Usage == nil || usage[1].Usage.Input != 273 {
		t.Errorf("usage events = %+v / %+v", usage[0].Usage, usage[1].Usage)
	}
}
