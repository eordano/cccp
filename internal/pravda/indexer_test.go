package pravda

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eordano/cccp/internal/store"
)

func openIndexStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func appendLines(t *testing.T, st *store.Store, host, path string, lines ...string) {
	t.Helper()
	var ll []store.LogLine
	for _, l := range lines {
		sum := sha256.Sum256([]byte(l))
		ll = append(ll, store.LogLine{Path: path, LineHash: hex.EncodeToString(sum[:]), Contents: []byte(l), FirstHost: host})
	}
	if _, err := st.AppendLogLines(context.Background(), ll); err != nil {
		t.Fatal(err)
	}
}

func jl(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const (
	rootPath  = "projects/-srv-repo/aaaa-1111.jsonl"
	subPath   = "projects/-srv-repo/aaaa-1111/subagents/agent-b2.jsonl"
	codexRoot = "agent/codex/sessions/2026/10/01/rollout-2026-10-01T10-00-00-parent-thread.jsonl"
	codexKid  = "agent/codex/sessions/2026/10/01/rollout-2026-10-01T10-05-00-child-thread.jsonl"
)

func claudeRootLines() []string {
	asst := func(id, ts string, content []map[string]any) string {
		return jl(map[string]any{"type": "assistant", "sessionId": "aaaa-1111", "cwd": "/home/alice/repo", "gitBranch": "main", "timestamp": ts,
			"message": map[string]any{"id": id, "model": "claude-opus-5-5", "role": "assistant", "content": content,
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 100, "cache_read_input_tokens": 1000}}})
	}
	return []string{
		jl(map[string]any{"type": "user", "sessionId": "aaaa-1111", "cwd": "/home/alice/repo", "gitBranch": "main", "version": "2.1.0", "timestamp": "2026-10-01T10:00:00Z",
			"message": map[string]any{"role": "user", "content": "fix the memory note"}}),
		jl(map[string]any{"type": "attachment", "timestamp": "2026-10-01T10:00:00Z", "attachment": map[string]any{"type": "instructions",
			"files": []map[string]any{{"path": "/home/alice/.claude/projects/-srv-repo/memory/MEMORY.md", "type": "AutoMem"}, {"path": "/srv/repo/CLAUDE.md", "type": "Project"}}}}),
		jl(map[string]any{"type": "attachment", "timestamp": "2026-10-01T10:00:00Z", "attachment": map[string]any{"type": "skill_listing", "names": []string{"review", "deploy"}}}),
		asst("msg_1", "2026-10-01T10:00:05Z", []map[string]any{{"type": "thinking", "thinking": "hm"}}),
		asst("msg_1", "2026-10-01T10:00:05Z", []map[string]any{{"type": "tool_use", "id": "tu1", "name": "Write",
			"input": map[string]any{"file_path": "/home/alice/.claude/projects/-srv-repo/memory/note.md", "content": "x"}}}),
		jl(map[string]any{"type": "user", "sessionId": "aaaa-1111", "timestamp": "2026-10-01T10:00:06Z",
			"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "tu1", "content": "denied", "is_error": true}}}}),
		asst("msg_2", "2026-10-01T10:01:00Z", []map[string]any{{"type": "tool_use", "id": "tu2", "name": "Agent", "input": map[string]any{"prompt": "look"}}}),
		asst("msg_3", "2026-10-01T10:02:00Z", []map[string]any{{"type": "text", "text": "done"}}),
		jl(map[string]any{"type": "ai-title", "aiTitle": "Fix memory note", "timestamp": "2026-10-01T10:02:00Z"}),
	}
}

func subLines() []string {
	return []string{
		jl(map[string]any{"type": "user", "sessionId": "aaaa-1111", "agentId": "b2", "isSidechain": true, "timestamp": "2026-10-01T10:01:01Z",
			"message": map[string]any{"role": "user", "content": "look"}}),
		jl(map[string]any{"type": "assistant", "sessionId": "aaaa-1111", "agentId": "b2", "isSidechain": true, "timestamp": "2026-10-01T10:01:30Z",
			"message": map[string]any{"id": "msg_s1", "model": "claude-haiku-4-5", "content": []map[string]any{{"type": "text", "text": "found it"}},
				"usage": map[string]any{"input_tokens": 5, "output_tokens": 50}}}),
	}
}

func codexLines(id, parent string) []string {
	meta := map[string]any{"id": id, "cwd": "/home/alice/repo", "cli_version": "0.50.0", "model_provider": "openai",
		"git": map[string]any{"branch": "feat", "commit_hash": "abc123", "repository_url": "git@x:y.git"}}
	if parent != "" {
		meta["parent_thread_id"] = parent
		meta["thread_source"] = "guardian_review"
	}
	return []string{
		jl(map[string]any{"type": "session_meta", "timestamp": "2026-10-01T10:00:00Z", "payload": meta}),
		jl(map[string]any{"type": "turn_context", "timestamp": "2026-10-01T10:00:01Z", "payload": map[string]any{"model": "gpt-5.5", "effort": "high"}}),
		jl(map[string]any{"type": "response_item", "timestamp": "2026-10-01T10:00:02Z", "payload": map[string]any{"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "review " + id}}}}),
		jl(map[string]any{"type": "event_msg", "timestamp": "2026-10-01T10:00:09Z", "payload": map[string]any{"type": "token_count",
			"info": map[string]any{"last_token_usage": map[string]any{"input_tokens": 300, "cached_input_tokens": 100, "output_tokens": 40}}}}),
	}
}

func seedSessions(t *testing.T, st *store.Store) {
	appendLines(t, st, "host-a", rootPath, claudeRootLines()...)
	appendLines(t, st, "host-a", subPath, subLines()...)
	appendLines(t, st, "host-b", codexRoot, codexLines("parent-thread", "")...)
	appendLines(t, st, "host-b", codexKid, codexLines("child-thread", "parent-thread")...)
	appendLines(t, st, "host-a", "history.jsonl", `{"display":"x"}`)
}

func rowsByPath(t *testing.T, st *store.Store) map[string]store.SessionRow {
	t.Helper()
	rows, _, err := st.ListSessionRows(context.Background(), store.SessionQuery{All: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.SessionRow{}
	for _, r := range rows {
		out[r.Path] = r
	}
	return out
}

func TestIndexerFoldsSessions(t *testing.T) {
	st := openIndexStore(t)
	seedSessions(t, st)
	ix := &Indexer{Store: st}
	ctx := context.Background()
	if _, err := ix.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	rows := rowsByPath(t, st)
	if len(rows) != 4 {
		t.Fatalf("indexed %d sessions, want 4 (history.jsonl excluded): %v", len(rows), rows)
	}
	root := rows[rootPath]
	if root.ID != "aaaa-1111" || root.Agent != "claude" || root.Host != "host-a" || root.User != "alice" || root.GitBranch != "main" || root.Version != "2.1.0" {
		t.Errorf("root identity: %+v", root)
	}
	if root.Title != "Fix memory note" || root.FirstPrompt != "fix the memory note" {
		t.Errorf("title %q prompt %q", root.Title, root.FirstPrompt)
	}
	if root.Requests != 3 || root.InputTokens != 30 || root.OutputTokens != 300 || root.CacheRead != 3000 {
		t.Errorf("one message split over two lines must count once: requests=%d in=%d out=%d cr=%d", root.Requests, root.InputTokens, root.OutputTokens, root.CacheRead)
	}
	if root.Turns != 1 || root.ToolCalls != 2 || root.Errors != 1 {
		t.Errorf("turns=%d tools=%d errors=%d", root.Turns, root.ToolCalls, root.Errors)
	}
	sub := rows[subPath]
	if sub.ParentPath != rootPath || sub.ID != "aaaa-1111/agent-b2" || sub.Source != "subagent" {
		t.Errorf("sub-agent link: %+v", sub)
	}
	if sub.CostReported || sub.OutputTokens != 50 || !reflect.DeepEqual(sub.Models, []string{"claude-haiku-4-5"}) {
		t.Errorf("sub-agent usage: %+v", sub)
	}
	kid := rows[codexKid]
	if kid.ParentPath != codexRoot || kid.ParentSessionID != "parent-thread" || kid.ID != "child-thread" || kid.Source != "guardian_review" {
		t.Errorf("codex child thread link: %+v", kid)
	}
	if kid.GitCommit != "abc123" || kid.GitBranch != "feat" || !reflect.DeepEqual(kid.Models, []string{"gpt-5.5"}) {
		t.Errorf("codex facts: %+v", kid)
	}
	if kid.InputTokens != 200 || kid.CacheRead != 100 || kid.OutputTokens != 40 {
		t.Errorf("codex usage attributed: %+v", kid)
	}

	writes, err := st.WritesTo(ctx, "projects/-srv-repo/memory/note.md")
	if err != nil || len(writes) != 1 || writes[0].SessionPath != rootPath || writes[0].Host != "host-a" {
		t.Fatalf("writes: %v %v", writes, err)
	}
}

func TestIndexerRollup(t *testing.T) {
	st := openIndexStore(t)
	seedSessions(t, st)
	appendLines(t, st, "host-b", "agent/codex/sessions/2026/10/01/rollout-2026-10-01T10-06-00-grandchild.jsonl", codexLines("grandchild", "child-thread")...)
	ix := &Indexer{Store: st}
	ctx := context.Background()
	if _, err := ix.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	ru, err := st.Rollups(ctx, []string{rootPath, codexRoot})
	if err != nil {
		t.Fatal(err)
	}
	r := ru[rootPath]
	if r.Sessions != 2 || r.Descendants != 1 || r.CostUSD != 0 || r.OutputTokens != 350 || r.ToolCalls != 2 {
		t.Errorf("claude rollup adds counted sub-agent usage: %+v", r)
	}
	appendLines(t, st, "host-a", rootPath, jl(map[string]any{"type": "cost-state", "timestamp": "2026-10-01T10:03:00Z", "totalCostUSD": 1.5,
		"modelUsage": map[string]any{
			"claude-opus-5-5":  map[string]any{"inputTokens": 30, "outputTokens": 300, "cacheReadInputTokens": 3000, "costUSD": 1.0},
			"claude-haiku-4-5": map[string]any{"inputTokens": 5, "outputTokens": 50, "costUSD": 0.5}}}))
	if _, err := ix.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	ru, err = st.Rollups(ctx, []string{rootPath, codexRoot})
	if err != nil {
		t.Fatal(err)
	}
	if r := ru[rootPath]; r.CostUSD != 1.5 || r.OutputTokens != 350 || r.InputTokens != 35 {
		t.Errorf("a process-wide cost-state already covers its sub-agents: %+v", r)
	}
	c := ru[codexRoot]
	if c.Sessions != 3 || c.InputTokens != 600 {
		t.Errorf("codex rollup over parent_session_id: %+v", c)
	}
	roots, total, err := st.ListSessionRows(ctx, store.SessionQuery{})
	if err != nil || total != 2 || len(roots) != 2 {
		t.Fatalf("roots only by default: %d %v", total, err)
	}
}

func TestIndexerIncrementalMatchesRebuild(t *testing.T) {
	ctx := context.Background()
	inc := openIndexStore(t)
	ix := &Indexer{Store: inc}
	if _, err := ix.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	root, sub := claudeRootLines(), subLines()
	for i := 0; i < len(root); i++ {
		appendLines(t, inc, "host-a", rootPath, root[i])
		if i < len(sub) {
			appendLines(t, inc, "host-a", subPath, sub[i])
		}
		if i%2 == 0 {
			if _, err := ix.CatchUp(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendLines(t, inc, "host-b", codexRoot, codexLines("parent-thread", "")...)
	appendLines(t, inc, "host-b", codexKid, codexLines("child-thread", "parent-thread")...)
	if _, err := ix.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}

	full := openIndexStore(t)
	seedSessions(t, full)
	if _, err := (&Indexer{Store: full}).Rebuild(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	a, b := rowsByPath(t, inc), rowsByPath(t, full)
	for p, ra := range a {
		rb := b[p]
		ra.Cursor, rb.Cursor = 0, 0
		if !reflect.DeepEqual(ra, rb) {
			t.Errorf("%s differs:\nincremental %+v\nrebuild     %+v", p, ra, rb)
		}
	}
	if len(a) != len(b) {
		t.Errorf("incremental %d rows, rebuild %d", len(a), len(b))
	}
}

func TestIndexerRebuildResumesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openIndexStore(t)
	seedSessions(t, st)
	ix := &Indexer{Store: st}
	if _, err := st.ResetIndex(ctx, IndexVersion); err != nil {
		t.Fatal(err)
	}
	m, _, _ := st.IndexMeta(ctx)
	if _, err := ix.rebuildPath(ctx, m, codexRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Rebuild(ctx, false, nil); err != nil {
		t.Fatal(err)
	}
	once := rowsByPath(t, st)
	m, _, _ = st.IndexMeta(ctx)
	if m.RebuildActive || m.Cursor != m.RebuildHead {
		t.Fatalf("rebuild did not finish: %+v", m)
	}
	if _, err := ix.Rebuild(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	again := rowsByPath(t, st)
	if !reflect.DeepEqual(once, again) {
		t.Errorf("second rebuild differs:\n%+v\n%+v", once, again)
	}
	appendLines(t, st, "host-a", rootPath, jl(map[string]any{"type": "user", "timestamp": "2026-10-01T11:00:00Z", "message": map[string]any{"role": "user", "content": "one more"}}))
	if _, err := ix.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if r := rowsByPath(t, st)[rootPath]; r.Turns != 2 || r.LineCount != len(claudeRootLines())+1 {
		t.Errorf("follower after rebuild: turns=%d lines=%d", r.Turns, r.LineCount)
	}
}

func TestIndexerCASRejectsStaleFold(t *testing.T) {
	ctx := context.Background()
	st := openIndexStore(t)
	ix := &Indexer{Store: st}
	if _, err := ix.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	appendLines(t, st, "host-a", rootPath, claudeRootLines()...)
	m, _, _ := st.IndexMeta(ctx)
	lines, _ := st.LinesAfter(ctx, m.Cursor, 100, 1<<20)
	if err := ix.followBatch(ctx, m, lines); err != nil {
		t.Fatal(err)
	}
	if err := ix.followBatch(ctx, m, lines); !errors.Is(err, store.ErrIndexConflict) {
		t.Fatalf("second fold from the same cursor must conflict, got %v", err)
	}
	if r := rowsByPath(t, st)[rootPath]; r.Requests != 3 {
		t.Errorf("lines folded twice: %+v", r)
	}
}

func TestEnvCountsSplitClaudeMessageOnce(t *testing.T) {
	events, err := ParseJSONL(strings.NewReader(strings.Join(claudeRootLines(), "\n")))
	if err != nil {
		t.Fatal(err)
	}
	env := buildEnv(Normalize(events, nil))
	if env.Total.Output != 300 || env.Total.Turns != 3 {
		t.Errorf("env totals: %+v", env.Total)
	}
}

func TestIndexerHermesSessionMeta(t *testing.T) {
	st := openIndexStore(t)
	raw, err := os.ReadFile("fixtures/hermes-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	const root = "agent/hermes/transcripts/20260908_021500_ab12cd.jsonl"
	const kid = "agent/hermes/transcripts/20260908_021600_ffff.jsonl"
	appendLines(t, st, "host-h", root, strings.Split(strings.TrimSpace(string(raw)), "\n")...)
	appendLines(t, st, "host-h", kid,
		jl(map[string]any{"type": "session-meta", "sessionId": "20260908_021600_ffff", "cwd": "/home/dev/todo", "timestamp": "2026-09-08T02:16:00Z",
			"agentSource": "delegate", "parentSessionId": "20260908_021500_ab12cd", "version": "hermes"}),
		jl(map[string]any{"type": "user", "sessionId": "20260908_021600_ffff", "timestamp": "2026-09-08T02:16:01Z",
			"message": map[string]any{"role": "user", "content": "sub task"}}))
	if _, err := (&Indexer{Store: st}).CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := rowsByPath(t, st)
	r := rows[root]
	if r.Agent != "hermes" || r.ID != "20260908_021500_ab12cd" || r.User != "dev" || r.GitBranch != "main" || r.GitCommit == "" || r.Source != "cli" || r.Turns != 1 || r.ToolCalls == 0 {
		t.Errorf("hermes root: %+v", r)
	}
	if k := rows[kid]; k.ParentPath != root || k.Source != "delegate" {
		t.Errorf("hermes delegation link: %+v", k)
	}
}

func TestIndexerHermesOwnSpace(t *testing.T) {
	st := openIndexStore(t)
	raw, err := os.ReadFile("fixtures/hermes-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	const space = "hermes-buzz-fizz"
	const root = "agent/" + space + "/transcripts/20260908_021500_ab12cd.jsonl"
	const kid = "agent/" + space + "/transcripts/20260908_021600_ffff.jsonl"
	const shared = "agent/hermes/transcripts/20260908_021600_ffff.jsonl"
	appendLines(t, st, "host-h", root, strings.Split(strings.TrimSpace(string(raw)), "\n")...)
	kidLines := []string{
		jl(map[string]any{"type": "session-meta", "sessionId": "20260908_021600_ffff", "cwd": "/home/dev/todo", "timestamp": "2026-09-08T02:16:00Z",
			"agentSource": "delegate", "parentSessionId": "20260908_021500_ab12cd", "version": "hermes"}),
		jl(map[string]any{"type": "user", "sessionId": "20260908_021600_ffff", "timestamp": "2026-09-08T02:16:01Z",
			"message": map[string]any{"role": "user", "content": "remember this"}}),
		jl(map[string]any{"type": "assistant", "sessionId": "20260908_021600_ffff", "timestamp": "2026-09-08T02:16:02Z",
			"message": map[string]any{"id": "m1", "role": "assistant", "model": "apiary/auto", "content": []map[string]any{{"type": "tool_use", "id": "tu1", "name": "Write",
				"input": map[string]any{"file_path": "/var/lib/buzz/fizz/runtime/home/.hermes/memories/MEMORY.md", "content": "x"}}}}}),
	}
	appendLines(t, st, "host-h", kid, kidLines...)
	appendLines(t, st, "host-other", shared, kidLines[:2]...)
	if _, err := (&Indexer{Store: st}).CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := rowsByPath(t, st)
	r := rows[root]
	if r.Agent != space || r.ID != "20260908_021500_ab12cd" || r.Turns != 1 || r.ToolCalls == 0 {
		t.Errorf("own space root: %+v", r)
	}
	if k := rows[kid]; k.Agent != space || k.ParentPath != root {
		t.Errorf("a delegation resolves inside its own space: %+v", k)
	}
	if s := rows[shared]; s.Agent != "hermes" || s.ParentPath != "" {
		t.Errorf("the shared space never links into an own space: %+v", s)
	}
	writes, err := st.WritesTo(context.Background(), "agent/"+space+"/memories/MEMORY.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0].SessionPath != kid {
		t.Errorf("memory write lands in the own space: %+v", writes)
	}
	if w, _ := st.WritesTo(context.Background(), "agent/hermes/memories/MEMORY.md"); len(w) != 0 {
		t.Errorf("own space write attributed to the shared space: %+v", w)
	}
	if got := backgroundPrefixes(&r); len(got) != 3 || got[0] != "agent/"+space+"/SOUL.md" {
		t.Errorf("background prefixes = %v", got)
	}
	if kind, agent, name := classifyBackground("agent/" + space + "/memories/MEMORY.md"); kind != bgMemory || agent != space || name != "MEMORY.md" {
		t.Errorf("classifyBackground = %q %q %q", kind, agent, name)
	}
	if got := agentDisplayName(space); got != "Hermes · buzz-fizz" {
		t.Errorf("display name = %q", got)
	}
	if converterFor(root) == nil || !isListedSession(root) {
		t.Error("own space must use the hermes converter")
	}
	agents, err := st.LogAgents(context.Background())
	if err != nil || !reflect.DeepEqual(agents, []string{"hermes", space}) {
		t.Errorf("LogAgents = %v %v", agents, err)
	}
}
