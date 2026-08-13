package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
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
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
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
	s.logMu.Lock()
	defer s.logMu.Unlock()
	firstSeen := time.Now().UnixNano()
	if firstSeen <= s.lastFirstSeen {
		firstSeen = s.lastFirstSeen + 1
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO log_lines(path, line_hash, contents, suggested_time, first_seen, first_host)
		VALUES(?, ?, ?, ?, ?, ?)`,
		l.Path, l.LineHash, l.Contents, l.SuggestedTime, firstSeen, l.FirstHost)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		s.lastFirstSeen = firstSeen
	}
	return n > 0, nil
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
}

func (s *Store) LogPaths(ctx context.Context) ([]LogPathInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, COUNT(*) AS line_count,
		       MAX(suggested_time) AS last_time,
		       SUM(LENGTH(contents)) AS total_size
		FROM log_lines
		GROUP BY path
		ORDER BY last_time DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogPathInfo
	for rows.Next() {
		var lp LogPathInfo
		if err := rows.Scan(&lp.Path, &lp.LineCount, &lp.LastTime, &lp.TotalSize); err != nil {
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

func (s *Store) LogContents(ctx context.Context, path string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT contents FROM log_lines
		WHERE path = ?
		ORDER BY suggested_time ASC, first_seen ASC`, path)
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
