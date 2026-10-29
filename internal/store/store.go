package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations.sql
var migrations string

type Store struct {
	db *sql.DB

	logMu         sync.Mutex
	lastFirstSeen int64
}

type FileVersion struct {
	Path       string
	Version    int64
	ContentSHA *string
	Mode       uint32
	MtimeNS    int64
	HostID     string
	IngestedAt int64
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=cache_size(-32768)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.ExecContext(context.Background(), migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := backfillLogPaths(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("backfill log_paths: %w", err)
	}
	s := &Store{db: db}
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(MAX(first_seen), 0) FROM log_lines`).Scan(&s.lastFirstSeen); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func backfillLogPaths(db *sql.DB) error {
	ctx := context.Background()
	var summarized, lines int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM log_paths), EXISTS(SELECT 1 FROM log_lines)`).Scan(&summarized, &lines); err != nil {
		return err
	}
	if summarized == 1 || lines == 0 {
		return nil
	}
	slog.Info("summarizing log_lines into log_paths (one-time, minutes on a large store)")
	start := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO log_paths(path, line_count, last_time, total_size, first_seen, first_host)
		SELECT l.path, COUNT(*),
		       (SELECT MAX(t.suggested_time) FROM log_lines t WHERE t.path = l.path),
		       SUM(LENGTH(l.contents)), MIN(l.first_seen), l.first_host
		FROM log_lines l NOT INDEXED
		GROUP BY l.path`)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	slog.Info("log_paths summarized", "paths", n, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *Store) HasBlob(ctx context.Context, sha string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM blobs WHERE sha=?`, sha).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) PutBlob(ctx context.Context, sha string, bytes []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO blobs(sha, size, bytes) VALUES(?, ?, ?)`,
		sha, len(bytes), bytes)
	return err
}

func (s *Store) GetBlob(ctx context.Context, sha string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT bytes FROM blobs WHERE sha=?`, sha).Scan(&b)
	return b, err
}

func (s *Store) LatestVersion(ctx context.Context, path string) (*FileVersion, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT path, version, content_sha, mode, mtime_ns, host_id, ingested_at
		FROM file_versions WHERE path=? ORDER BY version DESC LIMIT 1`, path)
	var fv FileVersion
	var sha sql.NullString
	if err := row.Scan(&fv.Path, &fv.Version, &sha, &fv.Mode, &fv.MtimeNS, &fv.HostID, &fv.IngestedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if sha.Valid {
		fv.ContentSHA = &sha.String
	}
	return &fv, nil
}

func (s *Store) HostLatestSHA(ctx context.Context, path, hostID string, window int) (string, bool, error) {
	var sha sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT content_sha FROM (
			SELECT version, host_id, content_sha FROM file_versions
			WHERE path = ? ORDER BY version DESC LIMIT ?
		) WHERE host_id = ? ORDER BY version DESC LIMIT 1`, path, window, hostID).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return sha.String, true, nil
}

func (s *Store) AppendVersion(ctx context.Context, fv FileVersion) error {
	var sha any
	if fv.ContentSHA != nil {
		sha = *fv.ContentSHA
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO file_versions(path, version, content_sha, mode, mtime_ns, host_id, ingested_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		fv.Path, fv.Version, sha, fv.Mode, fv.MtimeNS, fv.HostID, fv.IngestedAt)
	return err
}

func (s *Store) UpsertHost(ctx context.Context, id, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO hosts(id, name, last_seen) VALUES(?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, last_seen=excluded.last_seen`,
		id, name, time.Now().UnixNano())
	return err
}

type CurrentEntry struct {
	Path       string
	ContentSHA string
	Mode       uint32
	MtimeNS    int64
}

type LogLine struct {
	Path          string
	LineHash      string
	Contents      []byte
	SuggestedTime int64
	FirstSeen     int64
	FirstHost     string
}

func (s *Store) AppendLogLine(ctx context.Context, l LogLine) (bool, error) {
	inserted, err := s.AppendLogLines(ctx, []LogLine{l})
	if err != nil {
		return false, err
	}
	return inserted[0], nil
}

func (s *Store) AppendLogLines(ctx context.Context, lines []LogLine) ([]bool, error) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO log_lines(path, line_hash, contents, suggested_time, first_seen, first_host)
		VALUES(?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	inserted := make([]bool, len(lines))
	last := s.lastFirstSeen
	for i, l := range lines {
		firstSeen := time.Now().UnixNano()
		if firstSeen <= last {
			firstSeen = last + 1
		}
		res, err := stmt.ExecContext(ctx, l.Path, l.LineHash, l.Contents, l.SuggestedTime, firstSeen, l.FirstHost)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			inserted[i] = true
			last = firstSeen
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.lastFirstSeen = last
	return inserted, nil
}

func (s *Store) LogLinesSince(ctx context.Context, since int64, yield func(LogLine) bool) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, line_hash, contents, suggested_time, first_seen, first_host
		FROM log_lines WHERE first_seen > ? ORDER BY first_seen ASC`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.Path, &l.LineHash, &l.Contents, &l.SuggestedTime, &l.FirstSeen, &l.FirstHost); err != nil {
			return err
		}
		if !yield(l) {
			return nil
		}
	}
	return rows.Err()
}

func (s *Store) CurrentSnapshot(ctx context.Context) ([]CurrentEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH latest AS (
			SELECT path, MAX(version) AS version FROM file_versions GROUP BY path
		)
		SELECT fv.path, fv.content_sha, fv.mode, fv.mtime_ns
		FROM file_versions fv JOIN latest USING (path, version)
		WHERE fv.content_sha IS NOT NULL
		ORDER BY fv.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CurrentEntry
	for rows.Next() {
		var e CurrentEntry
		var sha sql.NullString
		if err := rows.Scan(&e.Path, &sha, &e.Mode, &e.MtimeNS); err != nil {
			return nil, err
		}
		e.ContentSHA = sha.String
		out = append(out, e)
	}
	return out, rows.Err()
}

type LogPathInfo struct {
	Path      string
	LineCount int
	LastTime  int64
	TotalSize int64
	Host      string
}

func (s *Store) LogPaths(ctx context.Context) ([]LogPathInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.path, p.line_count, p.last_time, p.total_size, COALESCE(h.name, p.first_host)
		FROM log_paths p LEFT JOIN hosts h ON h.id = p.first_host
		ORDER BY p.last_time DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogPathInfo
	for rows.Next() {
		var lp LogPathInfo
		if err := rows.Scan(&lp.Path, &lp.LineCount, &lp.LastTime, &lp.TotalSize, &lp.Host); err != nil {
			return nil, err
		}
		out = append(out, lp)
	}
	return out, rows.Err()
}

func (s *Store) LogHead(ctx context.Context, path string, limit int) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT contents FROM log_lines
		WHERE path = ?
		ORDER BY suggested_time ASC, first_seen ASC
		LIMIT ?`, path, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) LogContents(ctx context.Context, path string) ([][]byte, int64, error) {
	return s.scanLines(ctx, `
		SELECT contents, first_seen FROM log_lines
		WHERE path = ?
		ORDER BY suggested_time ASC, first_seen ASC`, path)
}

func (s *Store) LogLinesAfter(ctx context.Context, path string, after int64) ([][]byte, int64, error) {
	return s.scanLines(ctx, `
		SELECT contents, first_seen FROM log_lines
		WHERE path = ? AND first_seen > ?
		ORDER BY first_seen ASC`, path, after)
}

func (s *Store) scanLines(ctx context.Context, query string, args ...any) ([][]byte, int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out [][]byte
	var cursor int64
	for rows.Next() {
		var b []byte
		var seen int64
		if err := rows.Scan(&b, &seen); err != nil {
			return nil, 0, err
		}
		out = append(out, b)
		if seen > cursor {
			cursor = seen
		}
	}
	return out, cursor, rows.Err()
}

type PathFacts struct {
	LineCount int
	Facts     []byte
}

func (s *Store) AllPathFacts(ctx context.Context) (map[string]PathFacts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, line_count, facts FROM path_facts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PathFacts{}
	for rows.Next() {
		var p string
		var pf PathFacts
		if err := rows.Scan(&p, &pf.LineCount, &pf.Facts); err != nil {
			return nil, err
		}
		out[p] = pf
	}
	return out, rows.Err()
}

func (s *Store) PutPathFacts(ctx context.Context, path string, lineCount int, facts []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO path_facts(path, line_count, facts) VALUES(?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET line_count=excluded.line_count, facts=excluded.facts`,
		path, lineCount, facts)
	return err
}

func (s *Store) DB() *sql.DB {
	return s.db
}
