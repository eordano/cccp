package sae

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const jobsSchema = `CREATE TABLE IF NOT EXISTS sae_jobs (
 encoder TEXT NOT NULL, id TEXT NOT NULL, record TEXT NOT NULL, text TEXT NOT NULL,
 state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, retry_at INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL,
 PRIMARY KEY(encoder,id)
) STRICT, WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS sae_jobs_ready ON sae_jobs(state,retry_at);`

type ScorerConfig struct {
	CaptureURL string
	ModelsDir  string
	MaxPending int
	HTTPClient *http.Client
}
type JobState struct {
	State    string    `json:"state"`
	Attempts int       `json:"attempts"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
	Error    string    `json:"error,omitempty"`
}
type OnDemand struct {
	ix      *Index
	cfg     ScorerConfig
	client  *http.Client
	mu      sync.Mutex
	once    sync.Once
	wake    chan struct{}
	model   *cpuEncoder
	modelID string
}

func NewOnDemand(ix *Index, cfg ScorerConfig) (*OnDemand, error) {
	u, err := url.Parse(cfg.CaptureURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || cfg.ModelsDir == "" {
		return nil, errors.New("SAE capture URL and models directory are required")
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 256
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 90 * time.Second}
	}
	if _, err := ix.db.Exec(jobsSchema); err != nil {
		return nil, err
	}
	return &OnDemand{ix: ix, cfg: cfg, client: cfg.HTTPClient, wake: make(chan struct{}, 1)}, nil
}
func (o *OnDemand) Supports(encoder string) bool {
	if !safeEncoderID(encoder) {
		return false
	}
	dir := filepath.Join(o.cfg.ModelsDir, encoder)
	for _, p := range []string{filepath.Join(dir, "sae.safetensors"), scalePath(dir)} {
		f, err := os.Stat(p)
		if err != nil || !f.Mode().IsRegular() {
			return false
		}
	}
	return true
}
func (o *OnDemand) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Ensure only persists/coalesces a job; model execution never runs in a renderer.
func (o *OnDemand) Ensure(ctx context.Context, enc string, record Record, text string) (JobState, error) {
	if record.ID == "" || record.ID != RecordID(record.Kind, text) {
		return JobState{}, errors.New("SAE record does not match text")
	}
	exists, err := o.ix.Get(ctx, enc, record.ID)
	if err != nil {
		return JobState{}, err
	}
	if exists != nil {
		return JobState{State: "complete"}, nil
	}
	if !o.Supports(enc) {
		return JobState{State: "unavailable", Error: "Dictionary scoring artifacts are unavailable."}, nil
	}
	if len(text) > MaxCaptureBytes {
		return JobState{State: "unavailable", Error: "Message exceeds the on-demand capture size limit."}, nil
	}
	known, err := o.ix.HasEncoder(ctx, enc)
	if err != nil {
		return JobState{}, err
	}
	if !known {
		return JobState{State: "unavailable", Error: "Dictionary is not imported."}, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	states, err := o.States(ctx, enc, []string{record.ID})
	if err != nil {
		return JobState{}, err
	}
	if state, ok := states[record.ID]; ok {
		return state, nil
	}
	var count int
	if err := o.ix.db.QueryRowContext(ctx, `SELECT count(*) FROM sae_jobs WHERE state IN ('pending','running') OR (state='failed' AND retry_at>0)`).Scan(&count); err != nil {
		return JobState{}, err
	}
	if count >= o.cfg.MaxPending {
		return JobState{State: "unavailable", RetryAt: time.Now().Add(5 * time.Second), Error: "Scoring queue is full. Try again shortly."}, nil
	}
	record.Features = nil
	record.Snippet = clip(text, SnippetChars)
	raw, err := json.Marshal(record)
	if err != nil {
		return JobState{}, err
	}
	_, err = o.ix.db.ExecContext(ctx, `INSERT OR IGNORE INTO sae_jobs(encoder,id,record,text,state,updated_at) VALUES(?,?,?,?,'pending',?)`, enc, record.ID, string(raw), text, time.Now().Unix())
	if err != nil {
		return JobState{}, err
	}
	o.signal()
	return JobState{State: "pending"}, nil
}
func (o *OnDemand) States(ctx context.Context, enc string, ids []string) (map[string]JobState, error) {
	out := map[string]JobState{}
	for start := 0; start < len(ids); start += 400 {
		end := min(start+400, len(ids))
		args := []any{enc}
		marks := ""
		for _, id := range ids[start:end] {
			if marks != "" {
				marks += ","
			}
			marks += "?"
			args = append(args, id)
		}
		rows, err := o.ix.db.QueryContext(ctx, `SELECT id,state,attempts,retry_at,error FROM sae_jobs WHERE encoder=? AND id IN (`+marks+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var state JobState
			var retry int64
			if err := rows.Scan(&id, &state.State, &state.Attempts, &retry, &state.Error); err != nil {
				rows.Close()
				return nil, err
			}
			if retry > 0 {
				state.RetryAt = time.Unix(retry, 0)
			}
			out[id] = state
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Start runs one worker. Interrupted jobs resume after process restart.
func (o *OnDemand) Start(ctx context.Context) { o.once.Do(func() { o.run(ctx) }) }
func (o *OnDemand) run(ctx context.Context) {
	_, err := o.ix.db.ExecContext(ctx, `UPDATE sae_jobs SET state='pending' WHERE state='running'`)
	if err != nil {
		slog.Error("SAE worker recovery failed", "reason", "database unavailable")
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		worked := o.runOne(ctx)
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-o.wake:
		}
	}
}

type scoringJob struct {
	enc, id, text string
	record        Record
	attempts      int
}

func (o *OnDemand) claim(ctx context.Context) (*scoringJob, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	tx, err := o.ix.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	j := &scoringJob{}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT encoder,id,record,text,attempts FROM sae_jobs WHERE state='pending' OR (state='failed' AND retry_at>0 AND retry_at<=?) ORDER BY updated_at LIMIT 1`, time.Now().Unix()).Scan(&j.enc, &j.id, &raw, &j.text, &j.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &j.record); err != nil {
		return nil, err
	}
	j.attempts++
	_, err = tx.ExecContext(ctx, `UPDATE sae_jobs SET state='running',attempts=?,updated_at=? WHERE encoder=? AND id=?`, j.attempts, time.Now().Unix(), j.enc, j.id)
	if err != nil {
		return nil, err
	}
	return j, tx.Commit()
}
func (o *OnDemand) runOne(ctx context.Context) bool {
	j, err := o.claim(ctx)
	if err != nil || j == nil {
		return false
	}
	started := time.Now()
	slog.Info("SAE scoring started", "encoder", j.enc, "record_id", j.id, "attempt", j.attempts)
	existing, err := o.ix.Get(ctx, j.enc, j.id)
	if err == nil && existing != nil {
		err = o.complete(ctx, j, existing.Features)
	} else if err == nil {
		var layer, width, k int
		err = o.ix.db.QueryRowContext(ctx, `SELECT layer,width,top_k FROM sae_encoders WHERE encoder=?`, j.enc).Scan(&layer, &width, &k)
		if err == nil && o.modelID != j.enc {
			o.modelID = ""
			o.model, err = loadCPUEncoder(o.cfg.ModelsDir, j.enc)
			if err == nil {
				o.modelID = j.enc
			}
		}
		if err == nil && (o.model.width != width || o.model.k != k) {
			err = errors.New("dictionary metadata and weights disagree")
		}
		if err == nil {
			var mean []float32
			mean, err = o.capture(ctx, j.text, layer)
			if err == nil {
				var acts []Activation
				acts, err = o.model.score(mean)
				if err == nil {
					err = o.complete(ctx, j, acts)
				}
			}
		}
	}
	if err == nil {
		slog.Info("SAE scoring completed", "encoder", j.enc, "record_id", j.id, "attempt", j.attempts, "duration_ms", time.Since(started).Milliseconds())
		return true
	}
	// Only fixed messages and HTTP status codes may enter durable state or logs.
	message, retryable := scoringFailure(err)
	var retry int64
	if retryable {
		retry = time.Now().Add(scoringBackoff(j.attempts)).Unix()
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var updateErr error
	if ctx.Err() != nil {
		_, updateErr = o.ix.db.ExecContext(cleanup, `UPDATE sae_jobs SET state='pending',attempts=attempts-1,updated_at=? WHERE encoder=? AND id=?`, time.Now().Unix(), j.enc, j.id)
		slog.Info("SAE scoring interrupted", "encoder", j.enc, "record_id", j.id, "attempt", j.attempts)
	} else {
		_, updateErr = o.ix.db.ExecContext(cleanup, `UPDATE sae_jobs SET state='failed',retry_at=?,error=?,updated_at=? WHERE encoder=? AND id=?`, retry, message, time.Now().Unix(), j.enc, j.id)
		slog.Warn("SAE scoring failed", "encoder", j.enc, "record_id", j.id, "attempt", j.attempts, "retryable", retryable, "retry_at", retry, "reason", message, "duration_ms", time.Since(started).Milliseconds())
	}
	if updateErr != nil {
		slog.Error("SAE job state update failed", "encoder", j.enc, "record_id", j.id, "reason", "database unavailable")
	}
	return true
}

func scoringBackoff(attempts int) time.Duration {
	return min(30*time.Minute, time.Duration(1<<min(max(attempts, 1), 6))*30*time.Second)
}

func scoringFailure(err error) (string, bool) {
	var captureErr *captureHTTPError
	if errors.As(err, &captureErr) {
		status := captureErr.Status
		return captureErr.Error(), status == 408 || status == 429 || status >= 500 && status <= 599
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "The selected dictionary is no longer available.", false
	}
	switch err.Error() {
	case "capture service unavailable":
		return err.Error(), true
	case "invalid capture text", "invalid dictionary identifier", "dictionary weights unavailable", "dictionary weight checksum mismatch", "invalid dictionary tensors", "invalid encoder dimensions", "invalid dictionary normalization", "invalid dictionary top-k", "original dictionary scales unavailable", "invalid dictionary scales", "non-finite dictionary value", "invalid dictionary deviation", "invalid dictionary scale", "capture dimension does not match dictionary", "non-finite capture value", "non-finite scored activation", "invalid capture endpoint", "invalid capture response", "dictionary metadata and weights disagree":
		return err.Error(), false
	}
	// Unrecognized failures normally originate in SQLite. Their text can contain
	// record data, so expose a constant reason and retry with bounded backoff.
	return "Scoring could not finish. The worker will retry.", true
}

func (o *OnDemand) complete(ctx context.Context, j *scoringJob, acts []Activation) error {
	tx, err := o.ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r := j.record
	raw, _ := json.Marshal(acts)
	// Do not rewrite dictionary labels, promotion, import timestamps or existing records.
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sae_records(encoder,id,kind,agent,host,session,time,path,line,snippet,features,norm) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, j.enc, j.id, r.Kind, r.Agent, r.Host, r.Session, r.Time, r.Path, r.Line, r.Snippet, string(raw), norm(acts))
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	if inserted > 0 {
		for _, a := range acts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sae_record_features(encoder,feature,id,pct) VALUES(?,?,?,?)`, j.enc, a.Feature, j.id, a.Pct); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sae_jobs SET state='complete',text='',error='',retry_at=0,updated_at=? WHERE encoder=? AND id=?`, time.Now().Unix(), j.enc, j.id); err != nil {
		return err
	}
	return tx.Commit()
}
