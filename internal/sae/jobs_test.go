package sae

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func writeTestTensors(t *testing.T, path string, values map[string][]float32, shapes map[string][]int) []byte {
	t.Helper()
	var data bytes.Buffer
	header := map[string]tensorInfo{}
	for key, v := range values {
		start := data.Len()
		for _, f := range v {
			if err := binary.Write(&data, binary.LittleEndian, f); err != nil {
				t.Fatal(err)
			}
		}
		header[key] = tensorInfo{Dtype: "F32", Shape: shapes[key], Offsets: [2]int{start, data.Len()}}
	}
	h, _ := json.Marshal(header)
	var raw bytes.Buffer
	binary.Write(&raw, binary.LittleEndian, uint64(len(h)))
	raw.Write(h)
	raw.Write(data.Bytes())
	if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}
func testOnDemand(t *testing.T, handler http.HandlerFunc) (*OnDemand, string, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ix, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	weight := filepath.Join(root, "weights")
	raw := writeTestTensors(t, weight, map[string][]float32{"encoder": {1, 0, 2, 0, 1, 1}, "encoder_bias": {0, 0, 0}, "mu": {1, 2}, "sd": {2, 2}, "top_k": {2}}, map[string][]int{"encoder": {2, 3}, "encoder_bias": {3}, "mu": {2}, "sd": {2}, "top_k": {1}})
	sum := sha256.Sum256(raw)
	enc := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, enc)
	os.Mkdir(dir, 0700)
	os.Rename(weight, filepath.Join(dir, "sae.safetensors"))
	writeTestTensors(t, filepath.Join(dir, "scale.safetensors"), map[string][]float32{"scale": {10, 10, 20}}, map[string][]int{"scale": {3}})
	_, err = db.Exec(`INSERT INTO sae_encoders VALUES (?,3,2,19,123,1,'original')`, enc)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: testCaptureTransport{handler: handler}}
	worker, err := NewOnDemand(ix, ScorerConfig{CaptureURL: "http://capture.test", ModelsDir: root, MaxPending: 2, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	return worker, enc, db
}
func captureOK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"layer":19,"hidden_size":2,"records":[{"tokens":2,"mean":[3,6]}]}`))
}
func recordFor(text string) Record {
	return Record{ID: RecordID("user", text), Kind: "user", Agent: "claude", Path: "projects/test.jsonl", Line: 9}
}
func TestOnDemandUsesOriginalScaleAndPersistsWithoutChangingDictionary(t *testing.T) {
	var calls atomic.Int32
	o, enc, db := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Texts []string `json:"texts"`
			Layer int      `json:"layer"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		if request.Layer != 19 || len(request.Texts) != 1 || request.Texts[0] != "one message" {
			t.Errorf("request: %+v", request)
		}
		captureOK(w, r)
	})
	ctx := context.Background()
	r := recordFor("one message")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, err := o.Ensure(ctx, enc, r, "one message")
			if err != nil || state.State != "pending" {
				t.Errorf("ensure: %+v %v", state, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 0 {
		t.Fatal("Ensure ran capture synchronously")
	}
	if !o.runOne(ctx) {
		t.Fatal("job did not run")
	}
	got, err := o.ix.Get(ctx, enc, r.ID)
	if err != nil || got == nil {
		t.Fatalf("missing persisted result: %v", err)
	}
	// z=[1,2], pre=[1,2,4], top2=[f2,f1]. Original scales produce20%,20%, not100%.
	if len(got.Features) != 2 || got.Features[0] != (Activation{Feature: 2, Pct: 20}) || got.Features[1] != (Activation{Feature: 1, Pct: 20}) {
		t.Fatalf("score: %+v", got.Features)
	}
	var count, current, at int
	var label string
	db.QueryRow(`SELECT count(*) FROM sae_record_features WHERE encoder=? AND id=?`, enc, r.ID).Scan(&count)
	db.QueryRow(`SELECT current,imported_at,label FROM sae_encoders WHERE encoder=?`, enc).Scan(&current, &at, &label)
	if count != 2 || current != 1 || at != 123 || label != "original" {
		t.Fatalf("metadata changed: %d %d %d %s", count, current, at, label)
	}
	state, err := o.Ensure(ctx, enc, r, "one message")
	if err != nil || state.State != "complete" || calls.Load() != 1 {
		t.Fatalf("cache: %+v %v", state, err)
	}
	var text string
	db.QueryRow(`SELECT text FROM sae_jobs`).Scan(&text)
	if text != "" {
		t.Fatal("completed queue retained full text")
	}
}
func TestOnDemandFailureSanitizedBackoffAndBoundedQueue(t *testing.T) {
	o, enc, db := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "SECRET TRANSCRIPT BODY", 500) })
	ctx := context.Background()
	for _, text := range []string{"one", "two"} {
		if _, err := o.Ensure(ctx, enc, recordFor(text), text); err != nil {
			t.Fatal(err)
		}
	}
	state, err := o.Ensure(ctx, enc, recordFor("three"), "three")
	if err != nil || state.State != "unavailable" {
		t.Fatalf("unbounded queue: %+v %v", state, err)
	}
	o.runOne(ctx)
	states, err := o.States(ctx, enc, []string{recordFor("one").ID, recordFor("two").ID})
	if err != nil {
		t.Fatal(err)
	}
	failed := ""
	for id, s := range states {
		if s.State == "failed" {
			failed = id
			if s.Attempts != 1 || !s.RetryAt.After(time.Now()) || s.Error != "capture service returned HTTP 500" {
				t.Fatalf("failure: %+v", s)
			}
		}
	}
	if failed == "" {
		t.Fatal("no failed job")
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM sae_records`).Scan(&count)
	if count != 0 {
		t.Fatal("failed inference stored an activation")
	}
	// Terminal jobs are not retried, regardless of attempt count.
	db.Exec(`UPDATE sae_jobs SET state='failed', attempts=5,retry_at=0`)
	if o.runOne(ctx) {
		t.Fatal("terminal job retried")
	}
}
func TestOnDemandRestartsInterruptedJob(t *testing.T) {
	o, enc, db := testOnDemand(t, captureOK)
	ctx := context.Background()
	r := recordFor("resume")
	o.Ensure(ctx, enc, r, "resume")
	db.Exec(`UPDATE sae_jobs SET state='running'`)
	restarted, err := NewOnDemand(o.ix, o.cfg)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { restarted.Start(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("interrupted durable job did not resume")
		case <-tick.C:
			record, err := o.ix.Get(ctx, enc, r.ID)
			if err == nil && record != nil {
				return
			}
		}
	}
}
func TestOnDemandRejectsUnavailableWeightsAndBadCapture(t *testing.T) {
	o, enc, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"layer":19,"hidden_size":1,"records":[{"tokens":2,"mean":[3]}]}`))
	})
	ctx := context.Background()
	r := recordFor("dimension")
	if o.Supports("../escape") {
		t.Fatal("accepted traversal")
	}
	state, err := o.Ensure(ctx, "../escape", r, "dimension")
	if err != nil || state.State != "unavailable" {
		t.Fatalf("unavailable: %+v %v", state, err)
	}
	o.Ensure(ctx, enc, r, "dimension")
	o.runOne(ctx)
	states, _ := o.States(ctx, enc, []string{r.ID})
	if states[r.ID].State != "failed" {
		t.Fatal(states)
	}
	got, _ := o.ix.Get(ctx, enc, r.ID)
	if got != nil {
		t.Fatal("dimension mismatch persisted")
	}
	weights := filepath.Join(o.cfg.ModelsDir, enc, "sae.safetensors")
	os.WriteFile(weights, []byte("wrong weights"), 0600)
	if _, err := loadCPUEncoder(o.cfg.ModelsDir, enc); err == nil {
		t.Fatal("accepted wrong dictionary weights")
	}
}

type testCaptureTransport struct{ handler http.HandlerFunc }

func (t testCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

func TestOnDemandRetryBecomesEligibleOnlyAfterBackoff(t *testing.T) {
	var calls int
	o, enc, db := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "do not persist this body", 503) })
	ctx := context.Background()
	r := recordFor("retry")
	o.Ensure(ctx, enc, r, "retry")
	if !o.runOne(ctx) || o.runOne(ctx) || calls != 1 {
		t.Fatal("failed request bypassed backoff")
	}
	db.Exec(`UPDATE sae_jobs SET retry_at=1`)
	if !o.runOne(ctx) || calls != 2 {
		t.Fatal("due retry did not run")
	}
	states, _ := o.States(ctx, enc, []string{r.ID})
	if states[r.ID].Attempts != 2 || states[r.ID].Error != "capture service returned HTTP 503" {
		t.Fatal(states)
	}
}

func TestSafeTensorInvalidOffsetsReturnError(t *testing.T) {
	st := &safetensors{header: map[string]tensorInfo{"encoder": {Dtype: "F32", Offsets: [2]int{-1, 4}}, "indices": {Dtype: "I64", Offsets: [2]int{0, 99}}}, data: make([]byte, 4)}
	if _, _, err := st.f32("encoder"); err == nil {
		t.Fatal("negative offset accepted")
	}
	if _, _, err := st.i64("indices"); err == nil {
		t.Fatal("past-end offset accepted")
	}
}

func TestOnDemandTransientFailureRecoversBeyondFiveAttempts(t *testing.T) {
	calls := 0
	o, enc, db := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 6 {
			http.Error(w, "private upstream response", 503)
			return
		}
		captureOK(w, r)
	})
	ctx := context.Background()
	r := recordFor("eventually ready")
	o.Ensure(ctx, enc, r, "eventually ready")
	for attempt := 1; attempt <= 7; attempt++ {
		if !o.runOne(ctx) {
			t.Fatalf("job was abandoned before attempt%d", attempt)
		}
		states, _ := o.States(ctx, enc, []string{r.ID})
		state := states[r.ID]
		if state.Attempts != attempt {
			t.Fatalf("attempt count: %+v", state)
		}
		if attempt < 7 {
			if state.State != "failed" || state.RetryAt.IsZero() {
				t.Fatalf("transient failure became terminal: %+v", state)
			}
			if o.runOne(ctx) {
				t.Fatal("backoff ignored")
			}
			db.Exec(`UPDATE sae_jobs SET retry_at=1 WHERE encoder=? AND id=?`, enc, r.ID)
		} else if state.State != "complete" {
			t.Fatalf("did not recover: %+v", state)
		}
	}
	got, err := o.ix.Get(ctx, enc, r.ID)
	if err != nil || got == nil {
		t.Fatalf("recovered activations missing: %v", err)
	}
}

func TestOnDemandPermanentCaptureFailureDoesNotRetry(t *testing.T) {
	calls := 0
	o, enc, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "private upstream response", 400) })
	ctx := context.Background()
	r := recordFor("bad request")
	o.Ensure(ctx, enc, r, "bad request")
	o.runOne(ctx)
	states, _ := o.States(ctx, enc, []string{r.ID})
	state := states[r.ID]
	if state.State != "failed" || !state.RetryAt.IsZero() || state.Error != "capture service returned HTTP 400" {
		t.Fatalf("permanent failure: %+v", state)
	}
	if o.runOne(ctx) || calls != 1 {
		t.Fatal("permanent failure retried")
	}
}

func TestScoringFailureClassificationAndBackoff(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 408, 413, 429, 500, 502, 503, 504} {
		message, retry := scoringFailure(&captureHTTPError{Status: code, Message: "PRIVATE MESSAGE"})
		want := code == 408 || code == 429 || code >= 500
		if retry != want || strings.Contains(message, "PRIVATE") {
			t.Fatalf("HTTP%d: %s retry=%v", code, message, retry)
		}
	}
	for _, tc := range []struct {
		err   error
		retry bool
	}{{errors.New("capture service unavailable"), true}, {errors.New("dictionary weight checksum mismatch"), false}, {errors.New("capture dimension does not match dictionary"), false}, {errors.New("SQLite operation contains PRIVATE MESSAGE"), true}, {sql.ErrNoRows, false}} {
		message, retry := scoringFailure(tc.err)
		if retry != tc.retry || strings.Contains(message, "PRIVATE") {
			t.Fatalf("classification: %s retry=%v", message, retry)
		}
	}
	if scoringBackoff(1) != time.Minute || scoringBackoff(100) != 30*time.Minute {
		t.Fatal("backoff is not bounded")
	}
}

func TestOnDemandLogsIDsWithoutMessageText(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	o, enc, _ := testOnDemand(t, captureOK)
	ctx := context.Background()
	secret := "PRIVATE FULL MESSAGE"
	r := recordFor(secret)
	o.Ensure(ctx, enc, r, secret)
	o.runOne(ctx)
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), r.ID) || !strings.Contains(logs.String(), "SAE scoring started") || !strings.Contains(logs.String(), "SAE scoring completed") {
		t.Fatalf("unexpected scoring log: %s", logs.String())
	}
}
