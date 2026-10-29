package sae

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS sae_encoders (
  encoder     TEXT PRIMARY KEY,
  width       INTEGER NOT NULL,
  top_k       INTEGER NOT NULL,
  layer       INTEGER NOT NULL,
  imported_at INTEGER NOT NULL,
  current     INTEGER NOT NULL DEFAULT 0,
  label       TEXT NOT NULL DEFAULT '',
  model       TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE TABLE IF NOT EXISTS sae_features (
  encoder     TEXT NOT NULL,
  feature     INTEGER NOT NULL,
  name        TEXT,
  description TEXT,
  stance      TEXT,
  confidence  INTEGER,
  PRIMARY KEY (encoder, feature)
) STRICT, WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS sae_records (
  encoder  TEXT NOT NULL,
  id       TEXT NOT NULL,
  kind     TEXT NOT NULL,
  agent    TEXT NOT NULL,
  host     TEXT NOT NULL,
  session  TEXT NOT NULL,
  time     TEXT NOT NULL,
  path     TEXT NOT NULL,
  line     INTEGER NOT NULL,
  snippet  TEXT NOT NULL,
  features TEXT NOT NULL,
  PRIMARY KEY (encoder, id)
) STRICT, WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS sae_records_path ON sae_records(encoder, path, line);
CREATE TABLE IF NOT EXISTS sae_record_features (
  encoder  TEXT NOT NULL,
  feature  INTEGER NOT NULL,
  id       TEXT NOT NULL,
  pct      INTEGER NOT NULL,
  PRIMARY KEY (encoder, feature, id)
) STRICT, WITHOUT ROWID;
`

const SnippetChars = 240

const (
	LegacyModel = "poolside/Laguna-XS-2.1-NVFP4"
	LegacyLayer = 19
)
const SimilarDefaultLimit = 40

type Index struct{ db *sql.DB }

type Feature struct {
	Feature     int
	Name        string
	Description string
	Stance      string
	Confidence  int
}

type Activation struct {
	Feature int `json:"f"`
	Pct     int `json:"p"`
}

type Record struct {
	ID       string
	Kind     string
	Agent    string
	Host     string
	Session  string
	Time     string
	Path     string
	Line     int
	Snippet  string
	Features []Activation
}

func New(db *sql.DB) (*Index, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("sae schema: %w", err)
	}
	_, _ = db.Exec(`ALTER TABLE sae_encoders ADD COLUMN label TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE sae_records ADD COLUMN norm REAL NOT NULL DEFAULT 0`)
	if _, err := db.Exec(`ALTER TABLE sae_encoders ADD COLUMN model TEXT NOT NULL DEFAULT ''`); err == nil {
		if _, err := db.Exec(`UPDATE sae_encoders SET model=? WHERE model=''`, LegacyModel); err != nil {
			return nil, fmt.Errorf("sae model backfill: %w", err)
		}
	}
	ix := &Index{db: db}
	if err := ix.backfillFeatureIndex(context.Background()); err != nil {
		return nil, fmt.Errorf("sae feature index: %w", err)
	}
	return ix, nil
}

func norm(acts []Activation) float64 {
	var n float64
	for _, a := range acts {
		n += float64(a.Pct) * float64(a.Pct)
	}
	return math.Sqrt(n)
}

func (ix *Index) backfillFeatureIndex(ctx context.Context) error {
	var indexed, records int
	if err := ix.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM sae_record_features), (SELECT count(*) FROM sae_records)`).Scan(&indexed, &records); err != nil {
		return err
	}
	if indexed > 0 || records == 0 {
		return nil
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT encoder, id, features FROM sae_records`)
	if err != nil {
		return err
	}
	type row struct {
		enc, id, fb string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.enc, &r.id, &r.fb); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insF, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO sae_record_features(encoder,feature,id,pct) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insF.Close()
	upd, err := tx.PrepareContext(ctx, `UPDATE sae_records SET norm=? WHERE encoder=? AND id=?`)
	if err != nil {
		return err
	}
	defer upd.Close()
	for _, r := range all {
		var acts []Activation
		_ = json.Unmarshal([]byte(r.fb), &acts)
		for _, a := range acts {
			if _, err := insF.ExecContext(ctx, r.enc, a.Feature, r.id, a.Pct); err != nil {
				return err
			}
		}
		if _, err := upd.ExecContext(ctx, norm(acts), r.enc, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StaleDaily lists labelled daily dictionaries older than keep that are not
// the tagging encoder. Nothing is deleted unless an operator asks for it.
func (ix *Index) StaleDaily(ctx context.Context, keep time.Duration) ([]Encoder, error) {
	cutoff := time.Now().Add(-keep).Unix()
	rows, err := ix.db.QueryContext(ctx, `SELECT encoder, model, layer, label, imported_at, (SELECT count(*) FROM sae_records r WHERE r.encoder=e.encoder) FROM sae_encoders e WHERE label LIKE 'daily-%' AND current=0 AND imported_at < ? ORDER BY imported_at`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Encoder
	for rows.Next() {
		var e Encoder
		var at int64
		if err := rows.Scan(&e.Encoder, &e.Model, &e.Layer, &e.Label, &at, &e.Records); err != nil {
			return nil, err
		}
		e.ImportedAt = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEncoder removes one dictionary: its records, feature rows, names and
// the encoder row. Only the explicit `cccp sae prune --yes` calls it.
func (ix *Index) DeleteEncoder(ctx context.Context, enc string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM sae_record_features WHERE encoder=?`,
		`DELETE FROM sae_records WHERE encoder=?`,
		`DELETE FROM sae_features WHERE encoder=?`,
		`DELETE FROM sae_encoders WHERE encoder=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, enc); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type Encoder struct {
	Encoder    string
	Model      string
	Width      int
	TopK       int
	Layer      int
	ImportedAt time.Time
	Current    bool
	Label      string
	Records    int
}

// HasEncoder checks dictionary membership without counting its records.
func (ix *Index) HasEncoder(ctx context.Context, encoder string) (bool, error) {
	var exists bool
	err := ix.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sae_encoders WHERE encoder=?)`, encoder).Scan(&exists)
	return exists, err
}

// EncoderCatalog loads dictionary metadata without scanning activation records.
// Records is -1 because the catalog deliberately does not calculate counts.
func (ix *Index) EncoderCatalog(ctx context.Context) ([]Encoder, error) {
	return ix.encoders(ctx, false)
}

func (ix *Index) Encoders(ctx context.Context) ([]Encoder, error) {
	return ix.encoders(ctx, true)
}

func (ix *Index) encoders(ctx context.Context, withCounts bool) ([]Encoder, error) {
	count := "-1"
	if withCounts {
		count = "(SELECT count(*) FROM sae_records r WHERE r.encoder=e.encoder)"
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT e.encoder, e.model, e.width, e.top_k, e.layer, e.imported_at, e.current, e.label, `+count+` FROM sae_encoders e ORDER BY e.imported_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Encoder
	for rows.Next() {
		var e Encoder
		var at int64
		var cur int
		if err := rows.Scan(&e.Encoder, &e.Model, &e.Width, &e.TopK, &e.Layer, &at, &cur, &e.Label, &e.Records); err != nil {
			return nil, err
		}
		e.ImportedAt = time.Unix(at, 0)
		e.Current = cur == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

type ImportOptions struct {
	StripPrefix string
	Model       string
	Layer       int
	Label       string
	Promote     bool
}

type encoderIdentity struct {
	Model string
	Layer int
}

func resolveIdentity(model string, layer *int, o ImportOptions) (encoderIdentity, error) {
	id := encoderIdentity{Model: model}
	switch {
	case model == "" && o.Model != "":
		id.Model = o.Model
	case model == "":
		id.Model = LegacyModel
	case o.Model != "" && o.Model != model:
		return id, fmt.Errorf("result.json names capture model %q but the import was told %q", model, o.Model)
	}
	switch {
	case layer == nil && o.Layer != 0:
		id.Layer = o.Layer
	case layer == nil:
		id.Layer = LegacyLayer
	case o.Layer != 0 && o.Layer != *layer:
		return id, fmt.Errorf("result.json names capture layer %d but the import was told %d", *layer, o.Layer)
	default:
		id.Layer = *layer
	}
	if id.Layer < 0 {
		return id, fmt.Errorf("invalid capture layer %d", id.Layer)
	}
	return id, nil
}

func RecordID(kind, text string) string {
	k := map[string]string{"user": "User", "assistant": "Assistant", "tool": "Tool"}[kind]
	if k == "" {
		k = "Event:" + kind
	}
	sum := sha256.Sum256([]byte(k + "\n" + strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:])
}

type importRecord struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Agent   string `json:"agent"`
	Host    string `json:"host"`
	Session string `json:"session"`
	Time    string `json:"time"`
	Source  string `json:"source"`
	Line    int    `json:"line"`
	Text    string `json:"text"`
}

type namesFile struct {
	Model   string `json:"model"`
	Encoder string `json:"encoder_sha256"`
	Names   []struct {
		Feature     int     `json:"feature"`
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Stance      *string `json:"stance"`
		Confidence  *int    `json:"confidence"`
	} `json:"names"`
}

func ImportWith(ctx context.Context, db *sql.DB, dir string, o ImportOptions) (int, error) {
	if _, err := New(db); err != nil {
		return 0, err
	}
	stripPrefix := o.StripPrefix
	var result struct {
		Encoder string `json:"encoder_sha256"`
		Model   string `json:"model"`
		Layer   *int   `json:"layer"`
		Config  struct {
			Width int `json:"width"`
			TopK  int `json:"top_k"`
		} `json:"config"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("result.json: %w", err)
	}
	ident, err := resolveIdentity(result.Model, result.Layer, o)
	if err != nil {
		return 0, err
	}
	var names namesFile
	if raw, err := os.ReadFile(filepath.Join(dir, "names.json")); err == nil {
		if err := json.Unmarshal(raw, &names); err != nil {
			return 0, fmt.Errorf("names.json: %w", err)
		}
	}
	st, err := readSafetensors(filepath.Join(dir, "scored.safetensors"))
	if err != nil {
		return 0, err
	}
	idx, shape, err := st.i64("indices")
	if err != nil {
		return 0, err
	}
	vals, _, err := st.f32("values")
	if err != nil {
		return 0, err
	}
	scale, _, err := st.f32("scale")
	if err != nil {
		return 0, err
	}
	k := shape[1]
	f, err := os.Open(filepath.Join(dir, "records.jsonl"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	enc := result.Encoder
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO sae_encoders(encoder,width,top_k,layer,imported_at,current,label,model) VALUES(?,?,?,?,?,0,?,?)`, enc, result.Config.Width, result.Config.TopK, ident.Layer, time.Now().Unix(), o.Label, ident.Model); err != nil {
		return 0, err
	}
	for _, n := range names.Names {
		if n.Name == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO sae_features(encoder,feature,name,description,stance,confidence) VALUES(?,?,?,?,?,?)`, enc, n.Feature, *n.Name, deref(n.Description), deref(n.Stance), derefInt(n.Confidence)); err != nil {
			return 0, err
		}
	}
	ins, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO sae_records(encoder,id,kind,agent,host,session,time,path,line,snippet,features,norm) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	insF, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO sae_record_features(encoder,feature,id,pct) VALUES(?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer insF.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	row := 0
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var r importRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return row, fmt.Errorf("records.jsonl line %d: %w", row+1, err)
		}
		if row >= len(idx)/k {
			return row, fmt.Errorf("more records than scored rows")
		}
		acts := make([]Activation, 0, k)
		for j := 0; j < k; j++ {
			fi, v := idx[row*k+j], vals[row*k+j]
			if v <= 0 {
				continue
			}
			pct := int(math.Round(float64(v) / float64(scale[fi]) * 100))
			if pct > 100 {
				pct = 100
			}
			acts = append(acts, Activation{Feature: int(fi), Pct: pct})
		}
		fb, _ := json.Marshal(acts)
		path := strings.TrimPrefix(r.Source, stripPrefix)
		if _, err := ins.ExecContext(ctx, enc, r.ID, r.Kind, r.Agent, r.Host, r.Session, r.Time, path, r.Line, clip(r.Text, SnippetChars), string(fb), norm(acts)); err != nil {
			return row, err
		}
		for _, a := range acts {
			if _, err := insF.ExecContext(ctx, enc, a.Feature, r.ID, a.Pct); err != nil {
				return row, err
			}
		}
		row++
	}
	if err := sc.Err(); err != nil {
		return row, err
	}
	if o.Promote {
		if _, err := tx.ExecContext(ctx, `UPDATE sae_encoders SET current = CASE WHEN encoder=? THEN 1 ELSE 0 END`, enc); err != nil {
			return row, err
		}
	}
	return row, tx.Commit()
}

func (ix *Index) Current(ctx context.Context) (string, error) {
	var enc string
	err := ix.db.QueryRowContext(ctx, `SELECT encoder FROM sae_encoders WHERE current=1 LIMIT 1`).Scan(&enc)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return enc, err
}

func (ix *Index) Features(ctx context.Context, enc string) (map[int]Feature, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT feature, COALESCE(name,''), COALESCE(description,''), COALESCE(stance,''), COALESCE(confidence,0) FROM sae_features WHERE encoder=?`, enc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]Feature{}
	for rows.Next() {
		var f Feature
		if err := rows.Scan(&f.Feature, &f.Name, &f.Description, &f.Stance, &f.Confidence); err != nil {
			return nil, err
		}
		out[f.Feature] = f
	}
	return out, rows.Err()
}

func (ix *Index) Get(ctx context.Context, enc, id string) (*Record, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT id,kind,agent,host,session,time,path,line,snippet,features FROM sae_records WHERE encoder=? AND id=?`, enc, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rs, err := scanRecords(rows)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}

func (ix *Index) ByPath(ctx context.Context, enc, path string) ([]Record, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT id,kind,agent,host,session,time,path,line,snippet,features FROM sae_records WHERE encoder=? AND path=? ORDER BY line`, enc, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func (ix *Index) ByIDs(ctx context.Context, enc string, ids []string) (map[string]Record, error) {
	out := map[string]Record{}
	for start := 0; start < len(ids); start += 400 {
		end := start + 400
		if end > len(ids) {
			end = len(ids)
		}
		args := []any{enc}
		marks := make([]string, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
			marks = append(marks, "?")
		}
		rows, err := ix.db.QueryContext(ctx, `SELECT id,kind,agent,host,session,time,path,line,snippet,features FROM sae_records WHERE encoder=? AND id IN (`+strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		rs, err := scanRecords(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out[r.ID] = r
		}
	}
	return out, nil
}

type Match struct {
	Record
	Score float64
}

func (ix *Index) Similar(ctx context.Context, enc string, target []Activation, kind string, limit int) ([]Match, error) {
	if len(target) == 0 {
		return nil, nil
	}
	tv := map[int]float64{}
	var tn float64
	args := []any{enc}
	marks := make([]string, 0, len(target))
	for _, a := range target {
		tv[a.Feature] = float64(a.Pct)
		tn += float64(a.Pct) * float64(a.Pct)
		args = append(args, a.Feature)
		marks = append(marks, "?")
	}
	q := `SELECT f.id, f.feature, f.pct, r.norm FROM sae_record_features f JOIN sae_records r ON r.encoder=f.encoder AND r.id=f.id WHERE f.encoder=? AND f.feature IN (` + strings.Join(marks, ",") + `)`
	if kind != "" {
		q += ` AND r.kind=?`
		args = append(args, kind)
	}
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type cand struct {
		dot, norm float64
	}
	cands := map[string]*cand{}
	for rows.Next() {
		var id string
		var feature, pct int
		var n float64
		if err := rows.Scan(&id, &feature, &pct, &n); err != nil {
			return nil, err
		}
		c := cands[id]
		if c == nil {
			c = &cand{norm: n}
			cands[id] = c
		}
		c.dot += tv[feature] * float64(pct)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	type scored struct {
		id    string
		score float64
	}
	ranked := make([]scored, 0, len(cands))
	for id, c := range cands {
		if c.dot <= 0 || c.norm <= 0 {
			continue
		}
		ranked = append(ranked, scored{id, c.dot / (math.Sqrt(tn) * c.norm)})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].id < ranked[j].id
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	ids := make([]string, len(ranked))
	for i, r := range ranked {
		ids[i] = r.id
	}
	recs, err := ix.ByIDs(ctx, enc, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Match, 0, len(ranked))
	for _, r := range ranked {
		if rec, ok := recs[r.id]; ok {
			out = append(out, Match{Record: rec, Score: r.score})
		}
	}
	return out, nil
}

func (ix *Index) FeatureCounts(ctx context.Context, enc string) (map[int]int, int, error) {
	counts := map[int]int{}
	rows, err := ix.db.QueryContext(ctx, `SELECT feature, count(*) FROM sae_record_features WHERE encoder=? GROUP BY feature`, enc)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var f, c int
		if err := rows.Scan(&f, &c); err != nil {
			rows.Close()
			return nil, 0, err
		}
		counts[f] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM sae_records WHERE encoder=?`, enc).Scan(&total); err != nil {
		return nil, 0, err
	}
	return counts, total, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanRecord(s scanner) (Record, error) {
	var r Record
	var fb string
	if err := s.Scan(&r.ID, &r.Kind, &r.Agent, &r.Host, &r.Session, &r.Time, &r.Path, &r.Line, &r.Snippet, &fb); err != nil {
		return r, err
	}
	_ = json.Unmarshal([]byte(fb), &r.Features)
	return r, nil
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClipSnippet is what the store keeps of a record's text; pushes can trim to it.
func ClipSnippet(s string) string { return clip(s, SnippetChars) }

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
