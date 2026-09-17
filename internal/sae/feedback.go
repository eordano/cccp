package sae

import (
	"context"
	"database/sql"
	"encoding/json"
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

func (ix *Index) VotesByRecord(ctx context.Context) (map[string]int, error) {
	if err := ensureFeedback(ix.db); err != nil {
		return nil, err
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT record_id, vote FROM feedback`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var v int
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
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
	Up   int
	Down int
}

func (ix *Index) FeatureVoteStats(ctx context.Context, enc string) (map[int]FeatureVotes, int, int, error) {
	votes, err := ix.VotesByRecord(ctx)
	if err != nil || len(votes) == 0 {
		return nil, 0, 0, err
	}
	ids := make([]string, 0, len(votes))
	for id := range votes {
		ids = append(ids, id)
	}
	recs, err := ix.ByIDs(ctx, enc, ids)
	if err != nil {
		return nil, 0, 0, err
	}
	out := map[int]FeatureVotes{}
	up, down := 0, 0
	for id, r := range recs {
		v := votes[id]
		if v > 0 {
			up++
		} else if v < 0 {
			down++
		}
		for _, a := range r.Features {
			fv := out[a.Feature]
			if v > 0 {
				fv.Up++
			} else if v < 0 {
				fv.Down++
			}
			out[a.Feature] = fv
		}
	}
	return out, up, down, nil
}
