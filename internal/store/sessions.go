package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type SessionRow struct {
	Path            string   `json:"path"`
	ID              string   `json:"id"`
	SessionID       string   `json:"session_id"`
	Agent           string   `json:"agent"`
	Host            string   `json:"host_id"`
	HostName        string   `json:"host"`
	User            string   `json:"user,omitempty"`
	CWD             string   `json:"cwd,omitempty"`
	Project         string   `json:"project,omitempty"`
	GitBranch       string   `json:"git_branch,omitempty"`
	GitCommit       string   `json:"git_commit,omitempty"`
	Version         string   `json:"version,omitempty"`
	Models          []string `json:"models"`
	StartedAt       int64    `json:"started_at"`
	EndedAt         int64    `json:"ended_at"`
	Turns           int      `json:"turns"`
	Requests        int      `json:"requests"`
	ToolCalls       int      `json:"tool_calls"`
	Errors          int      `json:"errors"`
	InputTokens     int64    `json:"input_tokens"`
	OutputTokens    int64    `json:"output_tokens"`
	CacheRead       int64    `json:"cache_read_tokens"`
	CacheCreation   int64    `json:"cache_creation_tokens"`
	CostUSD         float64  `json:"cost_usd"`
	CostReported    bool     `json:"cost_reported"`
	ParentPath      string   `json:"parent_path,omitempty"`
	ParentSessionID string   `json:"parent_session_id,omitempty"`
	Source          string   `json:"source,omitempty"`
	Title           string   `json:"title,omitempty"`
	FirstPrompt     string   `json:"first_prompt,omitempty"`
	LineCount       int      `json:"lines"`
	Cursor          int64    `json:"-"`
	State           []byte   `json:"-"`
}

type SessionWrite struct {
	FilePath    string `json:"file_path"`
	SessionPath string `json:"session_path"`
	TS          int64  `json:"ts"`
	Host        string `json:"host_id"`
	Tool        string `json:"tool"`
}

type IndexMeta struct {
	Version       int
	Gen           int64
	Cursor        int64
	RebuildActive bool
	RebuildHead   int64
	RebuildPath   string
	UpdatedAt     int64
}

var ErrIndexConflict = errors.New("session index: concurrent update")

const sessionCols = `s.path, s.id, s.session_id, s.agent, s.host, COALESCE(h.name, s.host), s.user, s.cwd, s.project,
	s.git_branch, s.git_commit, s.version, s.models, s.started_at, s.ended_at, s.turns, s.requests, s.tool_calls,
	s.errors, s.input_tokens, s.output_tokens, s.cache_read, s.cache_creation, s.cost_usd, s.cost_reported,
	s.parent_path, s.parent_session_id, s.source, s.title, s.first_prompt, s.line_count, s.cursor`

func scanSession(sc interface{ Scan(...any) error }, withState bool, extra ...any) (SessionRow, error) {
	var r SessionRow
	var models string
	var reported int
	dest := []any{&r.Path, &r.ID, &r.SessionID, &r.Agent, &r.Host, &r.HostName, &r.User, &r.CWD, &r.Project,
		&r.GitBranch, &r.GitCommit, &r.Version, &models, &r.StartedAt, &r.EndedAt, &r.Turns, &r.Requests, &r.ToolCalls,
		&r.Errors, &r.InputTokens, &r.OutputTokens, &r.CacheRead, &r.CacheCreation, &r.CostUSD, &reported,
		&r.ParentPath, &r.ParentSessionID, &r.Source, &r.Title, &r.FirstPrompt, &r.LineCount, &r.Cursor}
	if withState {
		dest = append(dest, &r.State)
	}
	dest = append(dest, extra...)
	if err := sc.Scan(dest...); err != nil {
		return r, err
	}
	r.CostReported = reported != 0
	_ = json.Unmarshal([]byte(models), &r.Models)
	if r.Models == nil {
		r.Models = []string{}
	}
	return r, nil
}

func (s *Store) IndexMeta(ctx context.Context) (IndexMeta, bool, error) {
	var m IndexMeta
	var active int
	err := s.db.QueryRowContext(ctx, `SELECT version, gen, cursor, rebuild_active, rebuild_head, rebuild_path, updated_at
		FROM session_index_meta WHERE id = 1`).Scan(&m.Version, &m.Gen, &m.Cursor, &active, &m.RebuildHead, &m.RebuildPath, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	m.RebuildActive = active != 0
	return m, err == nil, err
}

func (s *Store) ResetIndex(ctx context.Context, version int) (IndexMeta, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IndexMeta{}, err
	}
	defer tx.Rollback()
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(first_seen), 0) FROM log_lines`).Scan(&head); err != nil {
		return IndexMeta{}, err
	}
	for _, q := range []string{`DELETE FROM session_index`, `DELETE FROM session_writes`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return IndexMeta{}, err
		}
	}
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_index_meta(id, version, gen, cursor, rebuild_active, rebuild_head, rebuild_path, updated_at)
		VALUES(1, ?, 1, 0, 1, ?, '', ?)
		ON CONFLICT(id) DO UPDATE SET version=excluded.version, gen=gen+1, cursor=0, rebuild_active=1,
			rebuild_head=excluded.rebuild_head, rebuild_path='', updated_at=excluded.updated_at`, version, head, now); err != nil {
		return IndexMeta{}, err
	}
	if err := tx.Commit(); err != nil {
		return IndexMeta{}, err
	}
	m, _, err := s.IndexMeta(ctx)
	return m, err
}

func (s *Store) LogPathsAfter(ctx context.Context, after string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM log_paths WHERE path > ? ORDER BY path LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PathLinesBetween(ctx context.Context, path string, after, upTo int64, limit int) ([]LogLine, error) {
	return s.queryLines(ctx, `
		SELECT path, line_hash, contents, suggested_time, first_seen, first_host FROM log_lines
		WHERE path = ? AND first_seen > ? AND first_seen <= ? ORDER BY first_seen LIMIT ?`, path, after, upTo, limit)
}

func (s *Store) LinesAfter(ctx context.Context, after int64, limit int, maxBytes int64) ([]LogLine, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, line_hash, contents, suggested_time, first_seen, first_host FROM log_lines
		WHERE first_seen > ? ORDER BY first_seen LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogLine
	var size int64
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.Path, &l.LineHash, &l.Contents, &l.SuggestedTime, &l.FirstSeen, &l.FirstHost); err != nil {
			return nil, err
		}
		out = append(out, l)
		if size += int64(len(l.Contents)); size >= maxBytes {
			break
		}
	}
	return out, rows.Err()
}

func (s *Store) queryLines(ctx context.Context, q string, args ...any) ([]LogLine, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogLine
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.Path, &l.LineHash, &l.Contents, &l.SuggestedTime, &l.FirstSeen, &l.FirstHost); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) SessionStates(ctx context.Context, paths []string) (map[string]*SessionRow, error) {
	out := make(map[string]*SessionRow, len(paths))
	for len(paths) > 0 {
		n := min(len(paths), 400)
		chunk := paths[:n]
		paths = paths[n:]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, p := range chunk {
			args[i] = p
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+`, s.state FROM session_index s
			LEFT JOIN hosts h ON h.id = s.host WHERE s.path IN (`+marks+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r, err := scanSession(rows, true)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[r.Path] = &r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type IndexUpdate struct {
	Row      SessionRow
	Expected int64
}

func casRow(ctx context.Context, tx *sql.Tx, u IndexUpdate) error {
	r := u.Row
	models, _ := json.Marshal(r.Models)
	if r.Models == nil {
		models = []byte("[]")
	}
	vals := []any{r.ID, r.SessionID, r.Agent, r.Host, r.User, r.CWD, r.Project, r.GitBranch, r.GitCommit, r.Version,
		string(models), r.StartedAt, r.EndedAt, r.Turns, r.Requests, r.ToolCalls, r.Errors, r.InputTokens,
		r.OutputTokens, r.CacheRead, r.CacheCreation, r.CostUSD, boolInt(r.CostReported), r.ParentPath,
		r.ParentSessionID, r.Source, r.Title, r.FirstPrompt, r.LineCount, r.Cursor, r.State}
	var res sql.Result
	var err error
	if u.Expected < 0 {
		res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_index(id, session_id, agent, host, user, cwd,
			project, git_branch, git_commit, version, models, started_at, ended_at, turns, requests, tool_calls, errors,
			input_tokens, output_tokens, cache_read, cache_creation, cost_usd, cost_reported, parent_path,
			parent_session_id, source, title, first_prompt, line_count, cursor, state, path)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, append(vals, r.Path)...)
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE session_index SET id=?, session_id=?, agent=?, host=?, user=?, cwd=?,
			project=?, git_branch=?, git_commit=?, version=?, models=?, started_at=?, ended_at=?, turns=?, requests=?,
			tool_calls=?, errors=?, input_tokens=?, output_tokens=?, cache_read=?, cache_creation=?, cost_usd=?,
			cost_reported=?, parent_path=?, parent_session_id=?, source=?, title=?, first_prompt=?, line_count=?,
			cursor=?, state=? WHERE path=? AND cursor=?`, append(vals, r.Path, u.Expected)...)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrIndexConflict
	}
	return nil
}

func insertWrites(ctx context.Context, tx *sql.Tx, writes []SessionWrite) error {
	if len(writes) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO session_writes(file_path, session_path, ts, host, tool) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, w := range writes {
		if _, err := stmt.ExecContext(ctx, w.FilePath, w.SessionPath, w.TS, w.Host, w.Tool); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CommitFollow(ctx context.Context, m IndexMeta, updates []IndexUpdate, writes []SessionWrite, cursor int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE session_index_meta SET cursor=?, updated_at=?
		WHERE id=1 AND gen=? AND cursor=? AND rebuild_active=0`, cursor, time.Now().UnixNano(), m.Gen, m.Cursor)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrIndexConflict
	}
	for _, u := range updates {
		if err := casRow(ctx, tx, u); err != nil {
			return err
		}
	}
	if err := insertWrites(ctx, tx, writes); err != nil {
		return err
	}
	if err := resolveParents(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitRebuild(ctx context.Context, m IndexMeta, u *IndexUpdate, writes []SessionWrite, donePath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var gen int64
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT gen, rebuild_active FROM session_index_meta WHERE id=1`).Scan(&gen, &active); err != nil {
		return err
	}
	if gen != m.Gen || active == 0 {
		return ErrIndexConflict
	}
	if u != nil {
		if err := casRow(ctx, tx, *u); err != nil {
			return err
		}
	}
	if err := insertWrites(ctx, tx, writes); err != nil {
		return err
	}
	if donePath != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE session_index_meta SET rebuild_path=?, updated_at=?
			WHERE id=1 AND rebuild_path < ?`, donePath, time.Now().UnixNano(), donePath); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FinishRebuild(ctx context.Context, m IndexMeta) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE session_index_meta SET rebuild_active=0, cursor=rebuild_head, updated_at=?
		WHERE id=1 AND gen=? AND rebuild_active=1`, time.Now().UnixNano(), m.Gen)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrIndexConflict
	}
	if err := resolveParents(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func resolveParents(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE session_index AS c SET parent_path = COALESCE((
			SELECT p.path FROM session_index p
			WHERE (p.id = c.parent_session_id OR p.session_id = c.parent_session_id)
			  AND p.path != c.path AND p.agent = c.agent
			ORDER BY p.id = c.parent_session_id DESC, p.parent_path != '', p.started_at LIMIT 1), '')
		WHERE c.parent_path = '' AND c.parent_session_id != ''`)
	return err
}

type SessionQuery struct {
	Agent   string
	Host    string
	Project string
	Q       string
	Parent  string
	All     bool
	Since   int64
	Limit   int
	Offset  int
}

func (s *Store) ListSessionRows(ctx context.Context, q SessionQuery) ([]SessionRow, int, error) {
	var where []string
	var args []any
	if q.Parent != "" {
		where = append(where, "s.parent_path = ?")
		args = append(args, q.Parent)
	} else if !q.All {
		where = append(where, "s.parent_path = ''")
	}
	if q.Agent != "" {
		where = append(where, "s.agent = ?")
		args = append(args, q.Agent)
	}
	if q.Host != "" {
		where = append(where, "(s.host = ? OR h.name = ?)")
		args = append(args, q.Host, q.Host)
	}
	if q.Project != "" {
		where = append(where, "(s.project = ? OR s.cwd = ?)")
		args = append(args, q.Project, q.Project)
	}
	if q.Since > 0 {
		where = append(where, "s.ended_at >= ?")
		args = append(args, q.Since)
	}
	if q.Q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q.Q) + "%"
		where = append(where, `(s.title LIKE ? ESCAPE '\' OR s.first_prompt LIKE ? ESCAPE '\' OR s.cwd LIKE ? ESCAPE '\' OR s.id LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	from := ` FROM session_index s LEFT JOIN hosts h ON h.id = s.host` + cond
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+from+` ORDER BY s.ended_at DESC LIMIT ? OFFSET ?`,
		append(args, limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		r, err := scanSession(rows, false)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (s *Store) SessionRowByKey(ctx context.Context, key string) (*SessionRow, error) {
	for _, q := range []string{
		`s.path = ?`,
		`s.id = ? ORDER BY s.parent_path != '', s.started_at DESC`,
		`s.session_id = ? ORDER BY s.parent_path != '', s.started_at`,
	} {
		row := s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM session_index s LEFT JOIN hosts h ON h.id = s.host WHERE `+q+` LIMIT 1`, key)
		r, err := scanSession(row, false)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &r, nil
	}
	return nil, nil
}

type Rollup struct {
	Sessions      int     `json:"sessions"`
	Descendants   int     `json:"descendants"`
	ToolCalls     int     `json:"tool_calls"`
	Errors        int     `json:"errors"`
	Turns         int     `json:"turns"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CacheRead     int64   `json:"cache_read_tokens"`
	CacheCreation int64   `json:"cache_creation_tokens"`
	CostUSD       float64 `json:"cost_usd"`
	StartedAt     int64   `json:"started_at"`
	EndedAt       int64   `json:"ended_at"`
}

func (s *Store) Rollups(ctx context.Context, roots []string) (map[string]Rollup, error) {
	out := map[string]Rollup{}
	if len(roots) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(roots)), ",")
	args := make([]any, len(roots))
	for i, p := range roots {
		args[i] = p
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE tree(root, path, depth, covered) AS (
			SELECT path, path, 0, 0 FROM session_index WHERE path IN (`+marks+`)
			UNION ALL
			SELECT tree.root, c.path, tree.depth + 1,
				tree.covered OR (p.cost_reported = 1 AND p.agent = 'claude' AND c.agent = 'claude' AND c.source = 'subagent')
			FROM session_index c JOIN tree ON c.parent_path = tree.path JOIN session_index p ON p.path = tree.path
			WHERE tree.depth < 16
		)
		SELECT tree.root, count(*), sum(s.tool_calls), sum(s.errors), sum(s.turns),
			sum(CASE WHEN tree.covered THEN 0 ELSE s.input_tokens END),
			sum(CASE WHEN tree.covered THEN 0 ELSE s.output_tokens END),
			sum(CASE WHEN tree.covered THEN 0 ELSE s.cache_read END),
			sum(CASE WHEN tree.covered THEN 0 ELSE s.cache_creation END),
			sum(CASE WHEN tree.covered THEN 0 ELSE s.cost_usd END),
			min(NULLIF(s.started_at, 0)), max(s.ended_at)
		FROM tree JOIN session_index s ON s.path = tree.path GROUP BY tree.root`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		var r Rollup
		var started sql.NullInt64
		if err := rows.Scan(&root, &r.Sessions, &r.ToolCalls, &r.Errors, &r.Turns, &r.InputTokens, &r.OutputTokens,
			&r.CacheRead, &r.CacheCreation, &r.CostUSD, &started, &r.EndedAt); err != nil {
			return nil, err
		}
		r.StartedAt = started.Int64
		r.Descendants = r.Sessions - 1
		out[root] = r
	}
	return out, rows.Err()
}

func (s *Store) SessionsUnder(ctx context.Context, agent, prefix string) ([]SessionRow, error) {
	q := `SELECT ` + sessionCols + ` FROM session_index s LEFT JOIN hosts h ON h.id = s.host
		WHERE s.agent = ? AND s.parent_path = ''`
	args := []any{agent}
	if prefix != "" {
		q += ` AND s.path >= ? AND s.path < ?`
		args = append(args, prefix, prefix+"\xff")
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY s.started_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		r, err := scanSession(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SessionsActiveAt(ctx context.Context, host string, t, slack int64) ([]SessionRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM session_index s LEFT JOIN hosts h ON h.id = s.host
		WHERE s.host = ? AND s.started_at <= ? AND s.ended_at >= ? ORDER BY s.started_at DESC LIMIT 5`, host, t+slack, t-slack)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		r, err := scanSession(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) WritesTo(ctx context.Context, filePath string) ([]SessionWrite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file_path, session_path, ts, host, tool FROM session_writes
		WHERE file_path = ? ORDER BY ts`, filePath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionWrite
	for rows.Next() {
		var w SessionWrite
		if err := rows.Scan(&w.FilePath, &w.SessionPath, &w.TS, &w.Host, &w.Tool); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) FileVersions(ctx context.Context, path string) ([]FileVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, version, content_sha, mode, mtime_ns, host_id, ingested_at
		FROM file_versions WHERE path = ? ORDER BY version`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileVersion
	for rows.Next() {
		var fv FileVersion
		var sha sql.NullString
		if err := rows.Scan(&fv.Path, &fv.Version, &sha, &fv.Mode, &fv.MtimeNS, &fv.HostID, &fv.IngestedAt); err != nil {
			return nil, err
		}
		if sha.Valid {
			fv.ContentSHA = &sha.String
		}
		out = append(out, fv)
	}
	return out, rows.Err()
}

type VersionTimeline struct {
	Versions  []FileVersion
	Reviews   []Review
	GateSince int64
}

func (s *Store) Timeline(ctx context.Context, path string) (*VersionTimeline, error) {
	versions, err := s.FileVersions(ctx, path)
	if err != nil {
		return nil, err
	}
	tl := &VersionTimeline{Versions: versions}
	if len(versions) == 0 {
		return tl, nil
	}
	reviews, err := s.Reviews(ctx, path)
	if err != nil {
		return nil, err
	}
	tl.Reviews = reviews[path]
	if tl.GateSince, err = s.GateSince(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return tl, nil
}

func (tl *VersionTimeline) Legacy(v FileVersion) bool {
	return tl.GateSince == 0 || v.IngestedAt <= tl.GateSince
}

func (tl *VersionTimeline) EffectiveAt(host string, t int64) int {
	best, arrived := -1, int64(-1)
	consider := func(i int, at int64) {
		if at > arrived || (at == arrived && i > best) {
			best, arrived = i, at
		}
	}
	index := map[int64]int{}
	for i, v := range tl.Versions {
		index[v.Version] = i
		switch {
		case v.HostID == host:
			if at := min(v.MtimeNS, v.IngestedAt); at <= t {
				consider(i, at)
			}
		case tl.Legacy(v):
			if v.IngestedAt <= t {
				consider(i, v.IngestedAt)
			}
		}
	}
	var decided []Review
	for _, r := range tl.Reviews {
		if r.DecidedAt <= t {
			decided = append(decided, r)
		}
	}
	if ver, ok := EffectiveVersion(decided); ok {
		if i, found := index[ver]; found {
			for _, r := range decided {
				if r.Version == ver && r.Decision == DecisionApproved {
					consider(i, r.DecidedAt)
					break
				}
			}
		}
	}
	return best
}

func (s *Store) VersionAt(ctx context.Context, path, host string, t int64) (*FileVersion, error) {
	tl, err := s.Timeline(ctx, path)
	if err != nil {
		return nil, err
	}
	if i := tl.EffectiveAt(host, t); i >= 0 {
		return &tl.Versions[i], nil
	}
	return nil, nil
}

func (s *Store) VersionedPaths(ctx context.Context, prefix string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT path FROM file_versions WHERE path >= ? AND path < ? ORDER BY path`,
		prefix, prefix+"\xff")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) HostNames(ctx context.Context) (map[string]string, error) {
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

type IndexStats struct {
	Sessions int
	Roots    int
	Writes   int
	Lines    int64
	Paths    int
}

func (s *Store) IndexStats(ctx context.Context) (IndexStats, error) {
	var st IndexStats
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM session_index),
		(SELECT count(*) FROM session_index WHERE parent_path = ''),
		(SELECT count(*) FROM session_writes),
		(SELECT COALESCE(sum(line_count), 0) FROM session_index),
		(SELECT count(*) FROM log_paths)`).Scan(&st.Sessions, &st.Roots, &st.Writes, &st.Lines, &st.Paths)
	if err != nil {
		return st, fmt.Errorf("index stats: %w", err)
	}
	return st, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) LogAgents(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT substr(path, 7, instr(substr(path, 7), '/') - 1) FROM log_paths
		WHERE path >= 'agent/' AND path < 'agent0' AND instr(substr(path, 7), '/') > 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ClaudeProjectDirs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT substr(path, 1, 9 + instr(substr(path, 10), '/')) FROM session_index
		WHERE path >= 'projects/' AND path < 'projects0' AND instr(substr(path, 10), '/') > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
