package sae

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const feedbackSchema = `
CREATE TABLE IF NOT EXISTS feedback (
  path      TEXT NOT NULL,
  entry     INTEGER NOT NULL,
  record_id TEXT NOT NULL,
  kind      TEXT NOT NULL,
  vote      INTEGER NOT NULL,
  note      TEXT NOT NULL DEFAULT '',
  who       TEXT NOT NULL DEFAULT '',
  at        INTEGER NOT NULL,
  PRIMARY KEY (path, entry)
) STRICT, WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS feedback_record ON feedback(record_id);
`

type Feedback struct {
	Path     string `json:"path"`
	Entry    int    `json:"entry"`
	RecordID string `json:"record_id"`
	Kind     string `json:"kind"`
	Vote     int    `json:"vote"`
	Note     string `json:"note,omitempty"`
	Who      string `json:"who,omitempty"`
	At       int64  `json:"at"`
}

func ensureFeedback(db *sql.DB) error {
	_, err := db.Exec(feedbackSchema)
	return err
}

func (ix *Index) Vote(ctx context.Context, f Feedback) error {
	if err := ensureFeedback(ix.db); err != nil {
		return err
	}
	if f.Vote == 0 {
		_, err := ix.db.ExecContext(ctx, `DELETE FROM feedback WHERE path=? AND entry=?`, f.Path, f.Entry)
		return err
	}
	_, err := ix.db.ExecContext(ctx, `INSERT OR REPLACE INTO feedback(path,entry,record_id,kind,vote,note,who,at) VALUES(?,?,?,?,?,?,?,?)`,
		f.Path, f.Entry, f.RecordID, f.Kind, f.Vote, f.Note, f.Who, time.Now().Unix())
	return err
}

func (ix *Index) VotesByPath(ctx context.Context, path string) (map[int]int, error) {
	if err := ensureFeedback(ix.db); err != nil {
		return nil, err
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT entry, vote FROM feedback WHERE path=?`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var e, v int
		if err := rows.Scan(&e, &v); err != nil {
			return nil, err
		}
		out[e] = v
	}
	return out, rows.Err()
}

func (ix *Index) ExportFeedback(ctx context.Context, w io.Writer) (int, error) {
	if err := ensureFeedback(ix.db); err != nil {
		return 0, err
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT path,entry,record_id,kind,vote,note,who,at FROM feedback ORDER BY at`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	n := 0
	for rows.Next() {
		var f Feedback
		if err := rows.Scan(&f.Path, &f.Entry, &f.RecordID, &f.Kind, &f.Vote, &f.Note, &f.Who, &f.At); err != nil {
			return n, err
		}
		if err := enc.Encode(f); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

type FeatureVotes struct {
	Up   int `json:"up"`
	Down int `json:"down"`
}

func (v FeatureVotes) Ratio() float64 {
	if v.Up+v.Down == 0 {
		return -1
	}
	return float64(v.Up) / float64(v.Up+v.Down)
}

func (v FeatureVotes) RatioText() string {
	r := v.Ratio()
	if r < 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", r*100)
}

const netVotes = `WITH votes AS (SELECT record_id AS id, SUM(vote) AS net FROM feedback GROUP BY record_id HAVING net <> 0)`

func (ix *Index) FeatureVoteStats(ctx context.Context, enc string) (map[int]FeatureVotes, int, int, error) {
	if err := ensureFeedback(ix.db); err != nil {
		return nil, 0, 0, err
	}
	rows, err := ix.db.QueryContext(ctx, netVotes+`
SELECT CAST(json_extract(j.value,'$.f') AS INTEGER) AS feature, SUM(v.net > 0), SUM(v.net < 0)
FROM votes v JOIN sae_records r ON r.encoder=? AND r.id=v.id, json_each(r.features) j
GROUP BY feature`, enc)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	out := map[int]FeatureVotes{}
	for rows.Next() {
		var f int
		var v FeatureVotes
		if err := rows.Scan(&f, &v.Up, &v.Down); err != nil {
			return nil, 0, 0, err
		}
		out[f] = v
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	var up, down int
	err = ix.db.QueryRowContext(ctx, netVotes+`
SELECT COALESCE(SUM(v.net > 0),0), COALESCE(SUM(v.net < 0),0) FROM votes v JOIN sae_records r ON r.encoder=? AND r.id=v.id`, enc).Scan(&up, &down)
	return out, up, down, err
}

type FeatureFeedback struct {
	Feedback
	Snippet string
	Pct     int
}

func (f FeatureFeedback) When() string { return time.Unix(f.At, 0).UTC().Format("2006-01-02 15:04") }

func (ix *Index) FeedbackForFeature(ctx context.Context, enc string, feature, limit int) ([]FeatureFeedback, error) {
	if err := ensureFeedback(ix.db); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := ix.db.QueryContext(ctx, `
SELECT v.path, v.entry, v.record_id, v.kind, v.vote, v.note, v.who, v.at, r.snippet, CAST(json_extract(j.value,'$.p') AS INTEGER)
FROM feedback v JOIN sae_records r ON r.encoder=? AND r.id=v.record_id, json_each(r.features) j
WHERE json_extract(j.value,'$.f') = ?
ORDER BY v.at DESC, v.path, v.entry LIMIT ?`, enc, feature, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeatureFeedback
	for rows.Next() {
		var f FeatureFeedback
		if err := rows.Scan(&f.Path, &f.Entry, &f.RecordID, &f.Kind, &f.Vote, &f.Note, &f.Who, &f.At, &f.Snippet, &f.Pct); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
