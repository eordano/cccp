package pravda

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eordano/cccp/internal/sae"
	_ "modernc.org/sqlite"
)

func saeTestServer(t *testing.T) (*WatchServer, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ix, err := sae.New(db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO sae_encoders VALUES ('old',2,2,1,1,0,'old'),('new',2,2,1,2,1,'new')`)
	if err != nil {
		t.Fatal(err)
	}
	return &WatchServer{Sae: ix}, db
}
func TestSaePayloadKeepsMissingAndLowValues(t *testing.T) {
	s, db := saeTestServer(t)
	id := sae.RecordID("assistant", "low")
	_, err := db.Exec(`INSERT INTO sae_records VALUES ('new',?,'assistant','claude','h','s','','p',1,'low','[{"f":0,"p":12}]',0)`, id)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{Entries: []Entry{{Index: 1, Kind: KindMessage, Text: "low"}, {Index: 2, Kind: KindMessage, Text: "missing"}}}
	p := s.saeData(context.Background(), "new", sess)
	if p.Status.State != "partial" || p.Status.Eligible != 2 || p.Status.Tagged != 1 {
		t.Fatalf("status: %+v", p.Status)
	}
	if len(p.Messages) != 2 || !p.Messages[0].Processed || p.Messages[1].Processed || p.Messages[0].Values[0].Value != 12 {
		t.Fatalf("messages: %+v", p.Messages)
	}
	if len(p.Features) != 2 || p.Features[0].Count != 1 {
		t.Fatalf("catalog: %+v", p.Features)
	}
	data := s.decorateSae(context.Background(), sess, "new")
	setSaeSession(data, "dir/a?#.jsonl")
	parsed, err := url.Parse(data.Payload.RefreshURL)
	if err != nil || parsed.Path != "/sae/session/dir/a?#.jsonl" || parsed.Query().Get("encoder") != "new" {
		t.Fatalf("refresh: %s", data.Payload.RefreshURL)
	}
	var decoded saePayload
	if err := json.Unmarshal([]byte(data.JSON), &decoded); err != nil {
		t.Fatal(err)
	}
}
func TestSaeDoesNotTruncateLongSession(t *testing.T) {
	s, _ := saeTestServer(t)
	sess := &Session{}
	for i := 0; i < 501; i++ {
		sess.Entries = append(sess.Entries, Entry{Index: i, Kind: KindMessage, Text: "unprocessed"})
	}
	p := s.saeData(context.Background(), "new", sess)
	if len(p.Messages) != 501 || p.Status.Eligible != 501 {
		t.Fatalf("lost messages: %d", len(p.Messages))
	}
}
func TestSaeSelectionValidationAndUnavailable(t *testing.T) {
	s, _ := saeTestServer(t)
	r := httptest.NewRequest("GET", "/?encoder=old", nil)
	if got := s.chosenEncoder(r.Context(), r); got != "old" {
		t.Fatal(got)
	}
	r = httptest.NewRequest("GET", "/?encoder=unknown", nil)
	if got := s.chosenEncoder(r.Context(), r); got != "new" {
		t.Fatal(got)
	}
	r = httptest.NewRequest("POST", "/settings/sae-encoder", strings.NewReader("encoder=unknown"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("key", "sae-encoder")
	w := httptest.NewRecorder()
	s.handleSettings(w, r)
	if w.Code != 400 || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("invalid selection accepted: %d", w.Code)
	}
	p := (&WatchServer{}).saeData(context.Background(), "", nil)
	if p.Status.State != "unavailable" {
		t.Fatal(p.Status)
	}
}
func TestSaeReadFailureIsNotEmptyCoverage(t *testing.T) {
	s, db := saeTestServer(t)
	db.Close()
	p := s.saeData(context.Background(), "new", nil)
	if p.Status.State != "error" {
		t.Fatal(p.Status)
	}
}

func TestSaeDictionaryRedirectUpdatesExplicitEncoder(t *testing.T) {
	s, _ := saeTestServer(t)
	r := httptest.NewRequest("POST", "/settings/sae-encoder", strings.NewReader("encoder=old"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Referer", "http://example.com/sae/settings?encoder=new&session=abc")
	r.SetPathValue("key", "sae-encoder")
	w := httptest.NewRecorder()
	s.handleSettings(w, r)
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil || w.Code != 303 || u.Query().Get("encoder") != "old" || u.Query().Get("session") != "abc" {
		t.Fatalf("redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := saeSessionSettingsURL("/sae/settings", "a?#"); got != "/sae/settings?session=a%3F%23" {
		t.Fatal(got)
	}
}

func TestSaeSessionRenderingKeepsDictionaryOutOfCachedEntries(t *testing.T) {
	indexed, db := saeTestServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo.jsonl"), []byte(`{"type":"assistant","uuid":"one","message":{"role":"assistant","content":[{"type":"text","text":"example"}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	id := sae.RecordID("assistant", "example")
	if _, err := db.Exec(`INSERT INTO sae_records VALUES ('new',?,'assistant','claude','h','s','','demo.jsonl',1,'example','[{"f":0,"p":80}]',0)`, id); err != nil {
		t.Fatal(err)
	}
	s := NewWatchServer(NewFSStore(dir))
	s.Sae = indexed.Sae
	for _, encoder := range []string{"new", "old"} {
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, httptest.NewRequest("GET", "/session/demo.jsonl?encoder="+encoder, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `data-sae=`) {
			t.Fatalf("session render: %d %s", w.Code, w.Body.String())
		}
		cached, ok := s.parsed.getCachedSession("demo.jsonl", s.pathVersion("demo.jsonl"))
		if !ok || len(cached.Entries) != 1 || len(cached.Entries[0].Sae) != 0 {
			t.Fatalf("request-specific SAE tags leaked into the shared session cache: ok=%v cached=%+v", ok, cached)
		}
	}
	for _, path := range []string{"/sae/settings?encoder=new&session=demo.jsonl", "/sae/similar/" + id + "?encoder=new"} {
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "</html>") {
			t.Fatalf("incomplete page %s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestSaeEventInspectorShowsAllImportedActivations(t *testing.T) {
	indexed, db := saeTestServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo.jsonl"), []byte(`{"type":"assistant","uuid":"one","message":{"role":"assistant","content":[{"type":"text","text":"example"}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	id := sae.RecordID("assistant", "example")
	if _, err := db.Exec(`INSERT INTO sae_records VALUES ('new',?,'assistant','claude','h','s','','demo.jsonl',1,'example','[{"f":0,"p":80},{"f":1,"p":12}]',0)`, id); err != nil {
		t.Fatal(err)
	}
	s := NewWatchServer(NewFSStore(dir))
	s.Sae = indexed.Sae
	for _, encoder := range []string{"new", "old"} {
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, httptest.NewRequest("GET", "/session/demo.jsonl?event=1&encoder="+encoder, nil))
		body := w.Body.String()
		for _, want := range []string{`data-sae-event="1"`, `SAE activations`, `href="/session/demo.jsonl?encoder=` + encoder + `"`, `</html>`} {
			if w.Code != 200 || !strings.Contains(body, want) {
				t.Fatalf("encoder %s missing %q: %d %s", encoder, want, w.Code, body)
			}
		}
		if encoder == "new" {
			for _, want := range []string{`80%`, `12%`, `/sae/similar/` + id + `?encoder=new`} {
				if !strings.Contains(body, want) {
					t.Fatalf("missing activation %q", want)
				}
			}
		} else if !strings.Contains(body, "No stored result is available") || strings.Contains(body, ">12%</td>") {
			t.Fatal("dictionary without result must not inherit activations")
		}
	}
}

func TestSaeEventInspectorIneligibleEvent(t *testing.T) {
	s, _ := saeTestServer(t)
	entry := Entry{Index: 9, Kind: KindTurnDuration, DurationMs: 1000}
	data := s.decorateSae(context.Background(), &Session{Entries: []Entry{entry}}, "new")
	view := saeInspector(data, &entry)
	if view.JSON != "" || len(view.Tags) != 0 || !strings.Contains(view.Detail, "no readable text") {
		t.Fatalf("ineligible event: %+v", view)
	}
}

// The hot session path must not touch the global records table to enumerate
// dictionaries. Dropping it makes accidental correlated count queries fail.
func TestSaeSessionCatalogDoesNotScanRecords(t *testing.T) {
	s, db := saeTestServer(t)
	if _, err := db.Exec(`DROP TABLE sae_records`); err != nil {
		t.Fatal(err)
	}
	p := s.saeData(context.Background(), "new", &Session{})
	if p.Status.State != "ready" || len(p.Dictionaries) != 2 {
		t.Fatalf("metadata depended on records: %+v", p.Status)
	}
	if p.Status.Records != nil || p.Dictionary.Records != nil {
		t.Fatal("unknown record count represented as a measured count")
	}
	if _, err := s.Sae.Encoders(context.Background()); err == nil {
		t.Fatal("test must detect the inventory count query")
	}
}

func TestSaeSettingsStillReportsMeasuredCounts(t *testing.T) {
	s, _ := saeTestServer(t)
	p := s.saeDataWithCounts(context.Background(), "new", &Session{}, true)
	if p.Status.Records == nil || *p.Status.Records != 0 {
		t.Fatalf("missing measured empty count: %+v", p.Status)
	}
}

type fakeSaeExecutor struct {
	supported bool
	jobs      map[string]sae.JobState
	response  *sae.JobState
	ensured   []string
}

func (f *fakeSaeExecutor) Supports(string) bool { return f.supported }
func (f *fakeSaeExecutor) States(context.Context, string, []string) (map[string]sae.JobState, error) {
	return f.jobs, nil
}
func (f *fakeSaeExecutor) Ensure(_ context.Context, _ string, record sae.Record, _ string) (sae.JobState, error) {
	f.ensured = append(f.ensured, record.ID)
	state := sae.JobState{State: "pending"}
	if f.response != nil {
		return *f.response, nil
	}
	f.jobs[record.ID] = state
	return state, nil
}
func TestSaeRenderingQueuesOnlyRequestedMissingMessage(t *testing.T) {
	s, db := saeTestServer(t)
	cached := sae.RecordID("assistant", "cached")
	if _, err := db.Exec(`INSERT INTO sae_records VALUES ('new',?,'assistant','claude','h','s','','demo.jsonl',1,'cached','[{"f":0,"p":80}]',0)`, cached); err != nil {
		t.Fatal(err)
	}
	executor := &fakeSaeExecutor{supported: true, jobs: map[string]sae.JobState{}}
	s.SaeScorer = executor
	sess := &Session{Entries: []Entry{{Index: 1, Kind: KindMessage, Text: "cached"}, {Index: 2, Kind: KindMessage, Text: "missing"}, {Index: 3, Kind: KindMessage, Text: "other"}}}
	for _, event := range []string{"1", "2", "2"} {
		r := httptest.NewRequest("GET", "/session/demo.jsonl?event="+event, nil)
		s.enqueueSae(r.Context(), r, "demo.jsonl", sess, "new")
	}
	if len(executor.ensured) != 1 || executor.ensured[0] != sae.RecordID("assistant", "missing") {
		t.Fatalf("wrong enqueue: %v", executor.ensured)
	}
	p := s.saeData(context.Background(), "new", sess)
	if p.Status.Active != 1 || p.Messages[1].State != "pending" || p.Messages[2].State != "missing" {
		t.Fatalf("queue status: %+v %+v", p.Status, p.Messages)
	}
	for _, stage := range []struct {
		state    string
		attempts int
		active   int
	}{{"running", 1, 1}, {"failed", 2, 1}, {"failed", 5, 0}} {
		job := sae.JobState{State: stage.state, Attempts: stage.attempts, Error: "Scoring failed"}
		if stage.attempts == 2 {
			job.RetryAt = time.Now().Add(time.Second)
		}
		executor.jobs[executor.ensured[0]] = job
		p = s.saeData(context.Background(), "new", sess)
		if p.Messages[1].State != stage.state || p.Status.Active != stage.active {
			t.Fatalf("stage %+v: %+v", stage, p)
		}
	}
	executor.supported = false
	r := httptest.NewRequest("GET", "/session/demo.jsonl?event=3", nil)
	s.enqueueSae(r.Context(), r, "demo.jsonl", sess, "new")
	if len(executor.ensured) != 1 {
		t.Fatal("unsupported dictionary queued")
	}
	p = s.saeData(context.Background(), "new", sess)
	if p.Messages[2].State != "unavailable" {
		t.Fatal(p.Messages[2])
	}
}
func TestSaeRenderingQueuesOutsideVisibleWindow(t *testing.T) {
	s, _ := saeTestServer(t)
	executor := &fakeSaeExecutor{supported: true, jobs: map[string]sae.JobState{}}
	s.SaeScorer = executor
	sess := &Session{Entries: []Entry{{Index: 1, Kind: KindMessage, Text: "old"}, {Index: 2, Kind: KindMessage, Text: "visible"}}}
	r := httptest.NewRequest("GET", "/session/demo.jsonl?limit=1", nil)
	s.enqueueSae(r.Context(), r, "demo.jsonl", sess, "new")
	if len(executor.ensured) != 2 || executor.ensured[1] != sae.RecordID("assistant", "old") {
		t.Fatalf("old message omitted: %v", executor.ensured)
	}
}

func TestSaeEventInspectorQueuesAndRefreshesPersistedResult(t *testing.T) {
	indexed, db := saeTestServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo.jsonl"), []byte(`{"type":"assistant","uuid":"one","message":{"role":"assistant","content":[{"type":"text","text":"example"}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewWatchServer(NewFSStore(dir))
	s.Sae = indexed.Sae
	executor := &fakeSaeExecutor{supported: true, jobs: map[string]sae.JobState{}}
	s.SaeScorer = executor
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, httptest.NewRequest("GET", "/session/demo.jsonl?event=1&encoder=new", nil))
	if w.Code != 200 || len(executor.ensured) != 1 || !strings.Contains(w.Body.String(), "Queued for SAE scoring") {
		t.Fatalf("missing render queue: %d %s", w.Code, w.Body.String())
	}
	id := executor.ensured[0]
	if _, err := db.Exec(`INSERT INTO sae_records VALUES ('new',?,'assistant','claude','h','s','','demo.jsonl',1,'example','[{"f":0,"p":12}]',0)`, id); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.Routes().ServeHTTP(w, httptest.NewRequest("GET", "/sae/session/demo.jsonl?event=1&encoder=new", nil))
	var p saePayload
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(executor.ensured) != 1 || p.Status.Active != 0 || len(p.Messages) != 1 || !p.Messages[0].Processed || p.Messages[0].Values[0].Value != 12 || !strings.Contains(p.RefreshURL, "event=1") {
		t.Fatalf("cache refresh: %+v", p)
	}
}

func TestSaeReusesDictionaryForTextEvents(t *testing.T) {
	s, _ := saeTestServer(t)
	executor := &fakeSaeExecutor{supported: true, jobs: map[string]sae.JobState{}}
	s.SaeScorer = executor
	entries := []Entry{
		{Index: 1, Kind: KindTool, Name: "Bash", Input: json.RawMessage(`{"command":"go test ./..."}`), ResultContent: json.RawMessage(`[{"type":"text","text":"all tests passed"},{"type":"image","source":{"data":"DO_NOT_SCORE_IMAGE_BYTES"}}]`)},
		{Index: 2, Kind: KindThinking, Text: "Consider the alternatives"},
		{Index: 3, Kind: KindSystem, Text: "Context compacted"},
		{Index: 4, Kind: KindAgentResult, Summary: "Finished review", Result: "No issues"},
		{Index: 5, Kind: KindBashOutput, Text: "stdout", Stderr: "stderr"},
		{Index: 6, Kind: KindTurnDuration, DurationMs: 1000},
	}
	sess := &Session{Entries: entries}
	for i := range entries {
		r := httptest.NewRequest("GET", "/session/demo.jsonl?event="+fmt.Sprint(i+1), nil)
		s.enqueueSae(r.Context(), r, "demo.jsonl", sess, "new")
	}
	p := s.saeData(context.Background(), "new", sess)
	if len(executor.ensured) != 5 || len(p.Messages) != 5 || p.Status.Active != 5 {
		t.Fatalf("missing text events: %+v / %v", p.Status, executor.ensured)
	}
	toolText := saeText(&entries[0])
	for _, want := range []string{"Bash", "go test ./...", "all tests passed"} {
		if !strings.Contains(toolText, want) {
			t.Fatalf("missing %q from %q", want, toolText)
		}
	}
	if strings.Contains(toolText, "DO_NOT_SCORE_IMAGE_BYTES") {
		t.Fatal("image bytes must not enter text capture")
	}
	if p.Messages[0].ID != executor.ensured[0] || p.Messages[0].Preview != toolText {
		t.Fatal("queue and display must use the same text")
	}
	before := p.Messages[0].ID
	entries[0].ResultContent = json.RawMessage(`"tests failed"`)
	if sae.RecordID("tool", saeText(&entries[0])) == before {
		t.Fatal("new tool result must not reuse stale activations")
	}
	seen := map[string]bool{}
	for _, kind := range []string{"user", "assistant", "tool", "thinking", "system"} {
		id := sae.RecordID(kind, "same text")
		if seen[id] {
			t.Fatal("different event kinds share a cache key")
		}
		seen[id] = true
	}
	if saeKind(&Entry{Kind: KindThinking, Text: " \n"}) != "" {
		t.Fatal("whitespace is not readable text")
	}
}

func TestSaeBatchesFinishConversationBeyondWindow(t *testing.T) {
	s, db := saeTestServer(t)
	executor := &fakeSaeExecutor{supported: true, jobs: map[string]sae.JobState{}}
	s.SaeScorer = executor
	sess := &Session{}
	for i := 1; i <= 423; i++ {
		sess.Entries = append(sess.Entries, Entry{Index: i, Kind: KindMessage, Text: fmt.Sprintf("message %d", i)})
	}
	r := httptest.NewRequest("GET", "/session/demo.jsonl?limit=1", nil)
	for pass := 0; pass < 14; pass++ {
		before := len(executor.ensured)
		s.enqueueSae(r.Context(), r, "demo.jsonl", sess, "new")
		if len(executor.ensured)-before > 32 {
			t.Fatal("batch exceeded 32")
		}
		for _, id := range executor.ensured[before:] {
			if _, err := db.Exec(`INSERT INTO sae_records VALUES ('new',?,'assistant','claude','h','s','','demo.jsonl',1,'saved','[]',0)`, id); err != nil {
				t.Fatal(err)
			}
		}
		p := s.saeData(r.Context(), "new", sess)
		if p.Status.Saved+p.Status.Queued+p.Status.Running+p.Status.AwaitingRetry+p.Status.Failed+p.Status.NotRequested+p.Status.Unavailable != 423 {
			t.Fatalf("coverage does not sum: %+v", p.Status)
		}
		if p.Status.Saved < 423 && !p.Status.Poll {
			t.Fatalf("stopped before remainder queued: %+v", p.Status)
		}
		if pass == 13 && (p.Status.Saved != 423 || p.Status.Poll) {
			t.Fatalf("conversation not complete: %+v", p.Status)
		}
	}
	if len(executor.ensured) != 423 {
		t.Fatal(len(executor.ensured))
	}
}

func TestSaeCoverageSeparatesRetryAndTerminalStates(t *testing.T) {
	p := &saePayload{Status: saeStatus{Eligible: 7}, Messages: []saeMessage{
		{Processed: true, State: "complete"}, {State: "pending"}, {State: "running"}, {State: "failed", Retrying: true}, {State: "failed"}, {State: "missing"}, {State: "unavailable"},
	}}
	summarizeSaeStatus(p)
	if p.Status.Saved != 1 || p.Status.Queued != 1 || p.Status.Running != 1 || p.Status.AwaitingRetry != 1 || p.Status.Failed != 1 || p.Status.NotRequested != 1 || p.Status.Unavailable != 1 || !p.Status.Poll {
		t.Fatalf("coverage: %+v", p.Status)
	}
	for _, message := range []saeMessage{{State: "failed"}, {State: "unavailable"}} {
		p.Messages = []saeMessage{message}
		p.Status.Eligible = 1
		summarizeSaeStatus(p)
		if p.Status.Poll {
			t.Fatalf("terminal state polls: %+v", p.Status)
		}
	}
}

func TestSaeQueueFullWaitsButUnavailableStops(t *testing.T) {
	s, _ := saeTestServer(t)
	busy := sae.JobState{State: "unavailable", Error: "Scoring queue is full.", RetryAt: time.Now().Add(time.Second)}
	executor := &fakeSaeExecutor{supported: true, jobs: map[string]sae.JobState{}, response: &busy}
	s.SaeScorer = executor
	sess := &Session{Entries: []Entry{{Index: 1, Kind: KindMessage, Text: "message"}}}
	r := httptest.NewRequest("GET", "/session/demo.jsonl?event=1", nil)
	transient := s.enqueueSae(r.Context(), r, "demo.jsonl", sess, "new")
	p := s.saeData(r.Context(), "new", sess)
	applySaeTransient(p, transient)
	if !p.Status.Poll || p.Status.AwaitingRetry != 1 || p.Status.Unavailable != 0 {
		t.Fatalf("busy queue: %+v", p.Status)
	}
	busy.RetryAt = time.Time{}
	transient = s.enqueueSae(r.Context(), r, "demo.jsonl", sess, "new")
	p = s.saeData(r.Context(), "new", sess)
	applySaeTransient(p, transient)
	if p.Status.Poll || p.Status.Unavailable != 1 {
		t.Fatalf("terminal unavailable: %+v", p.Status)
	}
}
