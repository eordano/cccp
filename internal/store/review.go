package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
)

type Review struct {
	Path      string
	Version   int64
	Decision  string
	Reviewer  string
	Note      string
	DecidedAt int64
}

type VersionMeta struct {
	Agent    string
	Sessions string
}

type Precheck struct {
	Verdict   string
	Detail    string
	CheckedAt int64
}

var ErrNoVersion = errors.New("no such version")

func (s *Store) GateSince(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT value FROM review_meta WHERE key = 'gate_since'`).Scan(&v)
	return v, err
}

func scanVersion(row interface{ Scan(...any) error }) (*FileVersion, error) {
	var fv FileVersion
	var sha sql.NullString
	if err := row.Scan(&fv.Path, &fv.Version, &sha, &fv.Mode, &fv.MtimeNS, &fv.HostID, &fv.IngestedAt); err != nil {
		return nil, err
	}
	if sha.Valid {
		fv.ContentSHA = &sha.String
	}
	return &fv, nil
}

const versionCols = `path, version, content_sha, mode, mtime_ns, host_id, ingested_at`

func (s *Store) Version(ctx context.Context, path string, version int64) (*FileVersion, error) {
	fv, err := scanVersion(s.db.QueryRowContext(ctx,
		`SELECT `+versionCols+` FROM file_versions WHERE path = ? AND version = ?`, path, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoVersion
	}
	return fv, err
}

func (s *Store) queryVersions(ctx context.Context, query string, args ...any) ([]FileVersion, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileVersion
	for rows.Next() {
		fv, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *fv)
	}
	return out, rows.Err()
}

func (s *Store) Versions(ctx context.Context, path string) ([]FileVersion, error) {
	return s.queryVersions(ctx,
		`SELECT `+versionCols+` FROM file_versions WHERE path = ? ORDER BY version`, path)
}

func (s *Store) VersionsSince(ctx context.Context, since int64) ([]FileVersion, error) {
	return s.queryVersions(ctx,
		`SELECT `+versionCols+` FROM file_versions WHERE ingested_at > ? ORDER BY ingested_at DESC, path, version DESC`, since)
}

func (s *Store) LatestAsOf(ctx context.Context, cutoff int64) ([]CurrentEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH latest AS (
			SELECT path, MAX(version) AS version FROM file_versions
			WHERE ingested_at <= ? GROUP BY path
		)
		SELECT fv.path, fv.content_sha, fv.mode, fv.mtime_ns
		FROM file_versions fv JOIN latest USING (path, version)
		WHERE fv.content_sha IS NOT NULL
		ORDER BY fv.path`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CurrentEntry
	for rows.Next() {
		var e CurrentEntry
		if err := rows.Scan(&e.Path, &e.ContentSHA, &e.Mode, &e.MtimeNS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) AddReview(ctx context.Context, r Review) error {
	if r.Decision != DecisionApproved && r.Decision != DecisionRejected {
		return fmt.Errorf("decision must be %q or %q", DecisionApproved, DecisionRejected)
	}
	if _, err := s.Version(ctx, r.Path, r.Version); err != nil {
		return fmt.Errorf("%s v%d: %w", r.Path, r.Version, err)
	}
	if r.DecidedAt == 0 {
		r.DecidedAt = time.Now().UnixNano()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO file_reviews(path, version, decision, reviewer, note, decided_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		r.Path, r.Version, r.Decision, r.Reviewer, r.Note, r.DecidedAt)
	return err
}

func (s *Store) Reviews(ctx context.Context, path string) (map[string][]Review, error) {
	q := `SELECT path, version, decision, reviewer, note, decided_at FROM file_reviews`
	var args []any
	if path != "" {
		q += ` WHERE path = ?`
		args = append(args, path)
	}
	q += ` ORDER BY path, decided_at DESC, rowid DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Review{}
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.Path, &r.Version, &r.Decision, &r.Reviewer, &r.Note, &r.DecidedAt); err != nil {
			return nil, err
		}
		out[r.Path] = append(out[r.Path], r)
	}
	return out, rows.Err()
}

func EffectiveVersion(newestFirst []Review) (int64, bool) {
	rejected := map[int64]bool{}
	for _, r := range newestFirst {
		switch r.Decision {
		case DecisionRejected:
			rejected[r.Version] = true
		case DecisionApproved:
			if !rejected[r.Version] {
				return r.Version, true
			}
		}
	}
	return 0, false
}

func Decision(newestFirst []Review, version int64) string {
	for _, r := range newestFirst {
		if r.Version == version {
			return r.Decision
		}
	}
	return ""
}

func (s *Store) PutVersionMeta(ctx context.Context, path string, version int64, m VersionMeta) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO file_version_meta(path, version, agent, sessions) VALUES(?, ?, ?, ?)`,
		path, version, m.Agent, m.Sessions)
	return err
}

func (s *Store) VersionMeta(ctx context.Context, path string, version int64) (VersionMeta, error) {
	var m VersionMeta
	err := s.db.QueryRowContext(ctx,
		`SELECT agent, sessions FROM file_version_meta WHERE path = ? AND version = ?`, path, version).
		Scan(&m.Agent, &m.Sessions)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	return m, err
}

func (s *Store) PutPrecheck(ctx context.Context, path string, version int64, p Precheck) error {
	if p.CheckedAt == 0 {
		p.CheckedAt = time.Now().UnixNano()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO review_prechecks(path, version, verdict, detail, checked_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(path, version) DO UPDATE SET verdict = excluded.verdict, detail = excluded.detail, checked_at = excluded.checked_at`,
		path, version, p.Verdict, p.Detail, p.CheckedAt)
	return err
}

func (s *Store) Precheck(ctx context.Context, path string, version int64) (*Precheck, error) {
	var p Precheck
	err := s.db.QueryRowContext(ctx,
		`SELECT verdict, detail, checked_at FROM review_prechecks WHERE path = ? AND version = ?`, path, version).
		Scan(&p.Verdict, &p.Detail, &p.CheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

func (s *Store) ReviewHosts(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(name, '') FROM hosts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}
