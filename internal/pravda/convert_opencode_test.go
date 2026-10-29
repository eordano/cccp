package pravda

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func opencodeFixture(t *testing.T, name string) *Session {
	t.Helper()
	data, err := os.ReadFile("fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, raw := range bytes.Split(data, []byte{'\n'}) {
		for _, l := range convertOpencode(raw) {
			out.Write(l)
			out.WriteByte('\n')
		}
	}
	events, err := ParseJSONL(&out)
	if err != nil {
		t.Fatal(err)
	}
	return Normalize(events, nil)
}

func checkRendered(t *testing.T, sess *Session) map[string]int {
	t.Helper()
	kinds := map[string]int{}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		kinds[e.Kind]++
		if e.Kind == KindUnknown {
			t.Errorf("entry %d has unknown kind: %s", e.Index, e.Data)
			continue
		}
		if lookupTemplate(e) == "event-unknown" {
			t.Errorf("entry %d (%s) has no renderer", e.Index, e.Kind)
		}
		if html := string(renderEntryContent(e)); strings.Contains(html, "<!-- render error:") {
			t.Errorf("entry %d (%s): %s", e.Index, e.Kind, html)
		}
	}
	return kinds
}

func TestOpencodeConverterRegistered(t *testing.T) {
	if converters["opencode"] == nil {
		t.Fatal("no converter registered for opencode")
	}
	if got := agentOf("agent/opencode/transcripts/ses_x.jsonl"); got != "opencode" {
		t.Errorf("agentOf = %q", got)
	}
	for _, junk := range []string{"", "   ", "not json", "[1,2]"} {
		if got := convertOpencode([]byte(junk)); got != nil {
			t.Errorf("convertOpencode(%q) = %q, want nil", junk, got)
		}
	}
	line := []byte(` {"type":"user","uuid":"u1"} `)
	got := convertOpencode(line)
	if len(got) != 1 || string(got[0]) != `{"type":"user","uuid":"u1"}` {
		t.Errorf("convertOpencode passthrough = %q", got)
	}
}

func TestOpencodeRunNormalizes(t *testing.T) {
	sess := opencodeFixture(t, "opencode-run.jsonl")
	if got := extractTitleFromEvents(sess.Events); !strings.Contains(got, "Say hi, then use the read tool") {
		t.Errorf("title = %q, want the first user prompt", got)
	}
	kinds := checkRendered(t, sess)
	if kinds[KindUser] != 1 || kinds[KindTool] != 1 || kinds[KindMessage] != 1 {
		t.Errorf("kinds = %v, want 1 user, 1 tool, 1 message", kinds)
	}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		switch e.Kind {
		case KindUser:
			if e.CWD != "/home/dev/proj" {
				t.Errorf("user cwd = %q", e.CWD)
			}
		case KindTool:
			if e.Name != "Read" || !e.HasResult || e.IsError {
				t.Errorf("tool entry: name=%q hasResult=%v isError=%v", e.Name, e.HasResult, e.IsError)
			}
			if !strings.Contains(string(e.ResultContent), "hello from cccp") {
				t.Errorf("tool result = %s", e.ResultContent)
			}
			if _, ok := e.InputMap["file_path"]; !ok {
				t.Errorf("tool input lacks file_path alias: %s", e.Input)
			}
			if lookupTemplate(e) != "tool-Read" {
				t.Errorf("tool template = %q", lookupTemplate(e))
			}
		}
	}
}

func TestOpencodeSyntheticNormalizes(t *testing.T) {
	sess := opencodeFixture(t, "opencode-synthetic.jsonl")
	if got := extractTitleFromEvents(sess.Events); got != "harness" {
		t.Errorf("title = %q", got)
	}
	kinds := checkRendered(t, sess)
	want := map[string]int{KindUser: 1, KindMeta: 1, KindThinking: 1, KindTool: 3, KindMessage: 3, KindSystem: 1, KindMode: 2}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("kinds[%s] = %d, want %d (all: %v)", k, kinds[k], n, kinds)
		}
	}
	rawUsage := 0
	for _, e := range sess.Events {
		if e.Type != "usage" {
			continue
		}
		rawUsage++
		if e.Usage == nil || e.Usage.Input == 0 || e.CostUSD == 0 || e.Model == "" {
			t.Errorf("usage event not parsed: model=%q usage=%+v cost=%v", e.Model, e.Usage, e.CostUSD)
		}
	}
	if rawUsage != 2 {
		t.Errorf("raw usage events = %d, want 2", rawUsage)
	}
	errs, models := 0, []string{}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.RawSource != nil && e.RawSource.Type == "usage" {
			t.Errorf("entry %d rendered from a usage line", e.Index)
		}
		switch e.Kind {
		case KindTool:
			if e.IsError {
				errs++
				if e.Name != "Read" || !strings.Contains(string(e.ResultContent), "no such file") {
					t.Errorf("error tool entry: %s %s", e.Name, e.ResultContent)
				}
			}
			if e.Name == "Edit" && (string(e.InputMap["file_path"]) != `"/home/dev/proj/a.txt"` || lookupTemplate(e) != "tool-Edit") {
				t.Errorf("edit entry: input=%v template=%q", e.InputMap, lookupTemplate(e))
			}
		case KindMeta:
			if !strings.HasPrefix(e.Text, "<base>") {
				t.Errorf("meta entry text = %q", e.Text)
			}
		case KindSystem:
			if e.Subtype != "patch" || e.Text != "/home/dev/proj/a.txt" {
				t.Errorf("system entry: subtype=%q text=%q", e.Subtype, e.Text)
			}
		case KindMode:
			models = append(models, e.Mode)
		}
	}
	if errs != 1 {
		t.Errorf("error tool entries = %d, want 1", errs)
	}
	if strings.Join(models, ",") != "decent/llm-default,anthropic/claude-opus-5" {
		t.Errorf("model transitions = %v", models)
	}
}
