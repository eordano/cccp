package pravda

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func convertCodexFixture(t *testing.T) (*Session, int) {
	t.Helper()
	raw, err := os.ReadFile("fixtures/codex-rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	n := 0
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		for _, l := range convertCodex(line) {
			out.Write(l)
			out.WriteByte('\n')
			n++
		}
	}
	events, err := ParseJSONL(&out)
	if err != nil {
		t.Fatalf("parse converted: %v", err)
	}
	return Normalize(events, nil), n
}

func TestCodexConvertRendersEntries(t *testing.T) {
	sess, n := convertCodexFixture(t)
	if n != 62 {
		t.Errorf("converted lines = %d, want 62 from the 71 fixture lines", n)
	}
	if sess.Facts == nil {
		sess.Facts = factsFromEvents(sess.Events)
	}
	if sess.Facts.CWD == "" || sess.Facts.SessionID == "" {
		t.Errorf("session-meta must yield cwd and session id, got %+v", *sess.Facts)
	}
	if len(sess.Entries) == 0 {
		t.Fatal("no entries")
	}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.Kind == KindUnknown {
			t.Errorf("entry %d unknown kind: %s", e.Index, e.Data)
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

func TestCodexConvertShapes(t *testing.T) {
	sess, _ := convertCodexFixture(t)
	if got := extractTitleFromEvents(sess.Events); !strings.HasPrefix(got, "❯ reduce comments") {
		t.Errorf("title = %q, want the first real prompt", got)
	}
	var users, tools, interrupts, compacts, durations, failedTurns, thinking, modes, metas, goals, subagents int
	byName := map[string][]*Entry{}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		switch e.Kind {
		case KindUser:
			users++
			if strings.HasPrefix(e.Text, "<") {
				t.Errorf("environment context leaked as a user entry: %q", e.Text[:40])
			}
		case KindMeta:
			metas++
		case KindTool:
			tools++
			byName[e.Name] = append(byName[e.Name], e)
		case KindInterrupt:
			interrupts++
			if e.Text != "Turn aborted: interrupted" {
				t.Errorf("interrupt text = %q", e.Text)
			}
		case KindMode:
			if e.Subtype == "model-change" {
				modes++
			}
		case KindSystem:
			switch e.Subtype {
			case "compact_boundary":
				compacts++
				if !strings.Contains(e.Text, "window 1") {
					t.Errorf("compact text = %q, want the window number", e.Text)
				}
			case "goal":
				goals++
				if e.Status != "active" || !strings.HasPrefix(e.Text, "Make sure") {
					t.Errorf("goal entry = %q / %q", e.Text, e.Status)
				}
			case "subagent":
				subagents++
			}
		case KindTurnDuration:
			durations++
			if e.Level == "error" {
				failedTurns++
				if e.Text != "server_overloaded" || e.DurationMs != 2233000 {
					t.Errorf("failed turn = %q / %d", e.Text, e.DurationMs)
				}
			} else if e.DurationMs != 6379 {
				t.Errorf("durationMs = %d", e.DurationMs)
			}
		case KindThinking:
			thinking++
		}
	}
	if users != 1 {
		t.Errorf("user entries = %d, want 1", users)
	}
	if metas < 2 {
		t.Errorf("meta entries = %d, want the role prompt and <multi_agent_role> developer messages", metas)
	}
	if tools != 19 {
		t.Errorf("tool entries = %d, want 19 (22 emitted, 3 codexItem duplicates of the preceding tool dropped by the normalizer)", tools)
	}
	if interrupts != 1 || compacts != 1 || durations != 2 || failedTurns != 1 {
		t.Errorf("interrupts %d compacts %d durations %d failed %d, want 1/1/2/1", interrupts, compacts, durations, failedTurns)
	}
	if goals != 1 || subagents != 1 {
		t.Errorf("goal %d subagent %d entries, want 1 each", goals, subagents)
	}
	if modes < 1 {
		t.Error("turn_context/thread_settings_applied produced no model-change entry")
	}
	if thinking != 0 {
		t.Errorf("encrypted-only reasoning produced %d thinking entries", thinking)
	}

	bash := byName["Bash"]
	if len(bash) != 6 {
		t.Fatalf("Bash entries = %d, want 6 (2 legacy exec_command, 3 code-mode, 1 write_stdin)", len(bash))
	}
	var bashErr, stdin *Entry
	for _, e := range bash {
		if !e.HasResult || e.InputMap["command"] == nil {
			t.Errorf("Bash without command/result: %+v", e)
		}
		if e.IsError && strings.Contains(field(e, "command"), "pytest") {
			bashErr = e
		}
		if strings.HasPrefix(field(e, "description"), "stdin") {
			stdin = e
		}
	}
	if bashErr == nil {
		t.Error("code-mode exec_command with exit_code 1 not flagged is_error")
	} else if !strings.HasPrefix(resultText(bashErr.ResultContent), "Process exited with code 1\nFAILED") {
		t.Errorf("exit-code result not unwrapped: %q", resultText(bashErr.ResultContent))
	}
	if stdin == nil {
		t.Error("write_stdin not mapped to Bash with a stdin description")
	}

	edits := byName["Edit"]
	if len(edits) != 1 {
		t.Fatalf("Edit entries = %d, want 1: the FileChange item duplicates the apply_patch call and is dropped", len(edits))
	}
	for _, e := range edits {
		if basename(field(e, "file_path")) != "campaign_loop.py" || !strings.Contains(field(e, "new_string"), `out["status"] = "gating"`) ||
			!strings.Contains(field(e, "old_string"), "setdefault") {
			t.Errorf("Edit old/new not derived: %s", e.Input)
		}
	}
	if w := byName["Write"]; len(w) != 1 || !w[0].IsError || basename(field(w[0], "file_path")) != "inspect.py" {
		t.Errorf("failed FileChange add not a Write with is_error: %+v", w)
	}
	if r := byName["Read"]; len(r) != 1 || resultText(r[0].ResultContent) != "[image]" {
		t.Errorf("view_image/ImageView not mapped to Read with [image]: %+v", r)
	}
	if s := byName["WebSearch"]; len(s) != 1 || field(s[0], "query") != "codex rollout jsonl format" {
		t.Errorf("web__run/Extension not mapped to WebSearch: %+v", s)
	}
	if x := byName["exec"]; len(x) != 1 || field(x[0], "description") != "exec_command, clock__curr_time" || !x[0].HasResult {
		t.Errorf("multi-call script not kept as exec with a call list: %+v", x)
	}
	if g := byName["create_goal"]; len(g) != 1 || !g[0].IsError {
		t.Errorf("Script failed output not flagged is_error: %+v", g)
	}
	msgs := byName["send_message"]
	if len(msgs) != 2 || field(msgs[0], "message") != "Please pause lane 3 until the gate passes." ||
		!strings.HasPrefix(field(msgs[1], "message"), "[encrypted message") {
		t.Errorf("send_message inputs: %+v", msgs)
	}
	if q := byName["AskUserQuestion"]; len(q) != 1 {
		t.Errorf("request_user_input not renamed to AskUserQuestion: %d", len(q))
	} else {
		var in struct {
			Questions []struct {
				Header  string                   `json:"header"`
				Options []struct{ Label string } `json:"options"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(q[0].Input, &in)
		if len(in.Questions) != 1 || in.Questions[0].Header != "Release gate" || len(in.Questions[0].Options) != 2 {
			t.Errorf("AskUserQuestion input shape: %s", q[0].Input)
		}
		if got := resultText(q[0].ResultContent); got != "release_gate: Merge feature-off (Recommended)" {
			t.Errorf("answers not formatted: %q", got)
		}
	}
	for _, name := range []string{"spawn_agent", "wait_agent", "list_agents", "sleep"} {
		if e := byName[name]; len(e) != 1 || !e[0].HasResult {
			t.Errorf("%s not paired with its output: %+v", name, e)
		}
	}

	var agentMsgs int
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if e.Kind == KindMessage && strings.HasPrefix(e.Text, "**/root/campaign_") {
			agentMsgs++
			if strings.Contains(e.Text, "Message Type:") || strings.Contains(e.Text, "\n\n") {
				t.Errorf("agent_message header not stripped: %q", e.Text)
			}
		}
	}
	if agentMsgs != 2 {
		t.Errorf("inter-agent messages = %d, want 2", agentMsgs)
	}
}

func TestCodexSyntheticLines(t *testing.T) {
	raw, _ := os.ReadFile("fixtures/codex-rollout.jsonl")
	var usage, modelChange int
	var meta map[string]any
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		for _, l := range convertCodex(line) {
			var ev map[string]any
			_ = json.Unmarshal(l, &ev)
			switch ev["type"] {
			case "usage":
				usage++
				u, _ := ev["usage"].(map[string]any)
				if u["input_tokens"] == nil || u["cache_read_input_tokens"] == nil || u["output_tokens"] == nil {
					t.Errorf("usage line missing fields: %s", l)
				}
			case "model-change":
				modelChange++
				if ev["model"] != "gpt-5.6-sol" {
					t.Errorf("model-change model = %v", ev["model"])
				}
			case "session-meta":
				meta = ev
			}
		}
	}
	if usage != 2 {
		t.Errorf("usage lines = %d, want one per token_count", usage)
	}
	if modelChange != 2 {
		t.Errorf("model-change lines = %d, want turn_context + thread_settings_applied", modelChange)
	}
	for _, k := range []string{"cwd", "sessionId", "gitBranch", "gitCommit", "gitRepo", "modelProvider", "agentNickname", "agentPath", "model", "version"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("session-meta missing %s", k)
		}
	}
	if meta["gitRepo"] != "https://git.example.com/dev/proj.git" || meta["modelProvider"] != "openai" {
		t.Errorf("session-meta git/provider: %v %v", meta["gitRepo"], meta["modelProvider"])
	}
}

func TestCodexCustomToolInput(t *testing.T) {
	cases := []struct {
		script string
		name   string
		check  map[string]string
	}{
		{`const r = await tools.exec_command({"cmd":"ls -la","workdir":"/w"}); text(r.output);`, "Bash", map[string]string{"command": "ls -la", "workdir": "/w"}},
		{`text(await tools.exec_command({cmd:"rg -n 'x' .",workdir:'/w',yield_time_ms:1000,sandbox_permissions:"require_escalated",justification:"May I search?"}));`, "Bash", map[string]string{"command": "rg -n 'x' .", "description": "May I search?"}},
		{"const patch = \"*** Begin Patch\\n*** Add File: /w/new.txt\\n+hello\\n+world\\n*** End Patch\";\ntext(await tools.apply_patch(patch));\n", "Write", map[string]string{"file_path": "/w/new.txt", "content": "hello\nworld"}},
		{"text(await tools.apply_patch(`*** Begin Patch\n*** Delete File: /w/old.txt\n*** End Patch`));", "Edit", map[string]string{"file_path": "/w/old.txt", "old_string": "(deleted)"}},
		{`const r = await tools.write_stdin({session_id:42,chars:"q\n",yield_time_ms:1000}); text(r.output);`, "Bash", map[string]string{"command": "q\n", "description": "stdin → session 42"}},
		{`image((await tools.view_image({path:"/tmp/a.png",detail:"original"})).image_url);`, "Read", map[string]string{"file_path": "/tmp/a.png"}},
		{`const r = await tools.web__run({open:[{ref_id:"https://example.org/x"}],response_length:"long"}); text(r);`, "WebFetch", map[string]string{"url": "https://example.org/x"}},
		{`text(await tools.web__run({search_query:[{q:"a b"},{q:"c"}],response_length:"short"}));`, "WebSearch", map[string]string{"query": "a b | c"}},
		{`text(await tools.clock__curr_time({}));`, "clock__curr_time", map[string]string{"description": "current time"}},
		{`text(await tools.update_goal({status:"complete"}));`, "update_goal", map[string]string{"description": "update goal complete"}},
		{`const a = await tools.exec_command({"cmd":"ls"}); const b = await tools.read_file({"path":"x"});`, "exec", map[string]string{"description": "exec_command, read_file"}},
		{"// @exec: {\"yield_time_ms\": 1000}\nawait Promise.all([1,2].map(async i => text(await tools.exec_command({cmd:`echo ${i}`}))));", "exec", map[string]string{"description": "exec_command"}},
		{`const x = 1 + 2; text(x);`, "exec", map[string]string{"description": "script"}},
	}
	for _, c := range cases {
		name, in := codexCustomToolInput("exec", c.script)
		m, _ := in.(map[string]any)
		if name != c.name {
			t.Errorf("%q: name = %s, want %s (%v)", c.script, name, c.name, in)
			continue
		}
		for k, want := range c.check {
			if got, _ := m[k].(string); got != want {
				t.Errorf("%q: %s = %q, want %q", c.script, k, got, want)
			}
		}
		if name == "exec" && m["code"] == nil {
			t.Errorf("%q: exec fallback lost the code", c.script)
		}
	}
	name, in := codexToolInput("exec_command", `not json`)
	m, _ := in.(map[string]any)
	if name != "exec_command" || m["arguments"] != "not json" {
		t.Errorf("unparsable arguments: %s %v", name, in)
	}
}

func TestCodexJSValues(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{`{a:1,"b":'two',c:[true,null,{d:"e"}],}`, `{"a":1,"b":"two","c":[true,null,{"d":"e"}]}`},
		{"{s:\"a\\nb\\t\\\"q\\\" \\u00e9 \\x41\"}", `{"s":"a\nb\t\"q\" é A"}`},
		{"{ // comment\n k: /* c */ -2.5e1 }", `{"k":-25}`},
		{"`multi\nline`", `"multi\nline"`},
		{`{cmd}`, ``},
		{"`has ${x}`", ``},
	}
	for _, c := range cases {
		p := &jsParser{src: c.src}
		v, ok := p.value()
		if c.want == "" {
			if ok {
				t.Errorf("%q: expected failure, got %v", c.src, v)
			}
			continue
		}
		b, _ := json.Marshal(v)
		if string(b) != c.want {
			t.Errorf("%q: got %s, want %s", c.src, b, c.want)
		}
	}
	p := &jsParser{src: `const cmd = "ls"; const wd = '/w'; tools.exec_command({cmd, workdir: wd})`, pos: len(`const cmd = "ls"; const wd = '/w'; tools.exec_command(`)}
	v, ok := p.value()
	if b, _ := json.Marshal(v); !ok || string(b) != `{"cmd":"ls","workdir":"/w"}` {
		t.Errorf("identifier resolution: %v %s", ok, b)
	}
}

func TestCodexResultBlock(t *testing.T) {
	cases := []struct {
		output  string
		isErr   bool
		content string
	}{
		{`[{"type":"input_text","text":"Script completed\nWall time 0.2 seconds\nOutput:\n"},{"type":"input_text","text":"ok\n"}]`, false, "ok\n"},
		{`[{"type":"input_text","text":"Script failed\nWall time 0.0 seconds\nOutput:\n"},{"type":"input_text","text":"Script error:\nSyntaxError"}]`, true, "Script failed\nWall time 0.0 seconds\nScript error:\nSyntaxError"},
		{`[{"type":"input_text","text":"Script completed\nWall time 1 seconds\nOutput:\n"},{"type":"input_text","text":"{\"chunk_id\":\"a\",\"exit_code\":2,\"output\":\"boom\\n\"}"}]`, true, "Process exited with code 2\nboom"},
		{`[{"type":"input_text","text":"Script completed\nWall time 1 seconds\nOutput:\n"},{"type":"input_text","text":"{\"i\":0,\"result\":{\"status\":\"fulfilled\",\"value\":{\"chunk_id\":\"a\",\"exit_code\":0,\"output\":\"fine\"}}}"}]`, false, "fine"},
		{`[{"type":"input_text","text":"Script completed\nWall time 1 seconds\nOutput:\n"},{"type":"input_text","text":"{\"chunk_id\":\"a\",\"session_id\":77,\"output\":\"\"}"}]`, false, "[session 77 running]"},
		{`"Chunk ID: 1\nWall time: 0.1 seconds\nProcess exited with code 127\nOutput:\nnope"`, true, "Chunk ID: 1\nWall time: 0.1 seconds\nProcess exited with code 127\nOutput:\nnope"},
		{`"Script running with cell ID 31\nWall time 31.0 seconds\nOutput:\n"`, false, "Script running with cell ID 31\nWall time 31.0 seconds"},
		{`[{"type":"input_text","text":"Script completed\nWall time 1 seconds\nOutput:\n"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]`, false, "[image]"},
		{`"aborted by user after 12s"`, true, "aborted by user after 12s"},
	}
	for _, c := range cases {
		block := codexResultBlock("call_x", json.RawMessage(c.output))
		if _, ok := block["is_error"]; ok != c.isErr {
			t.Errorf("%s: is_error = %v, want %v", c.output, ok, c.isErr)
		}
		if got := block["content"]; got != c.content {
			t.Errorf("%s: content = %q, want %q", c.output, got, c.content)
		}
	}
}
