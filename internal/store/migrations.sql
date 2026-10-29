CREATE TABLE IF NOT EXISTS blobs (
  sha   TEXT PRIMARY KEY,
  size  INTEGER NOT NULL,
  bytes BLOB NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS file_versions (
  path        TEXT NOT NULL,
  version     INTEGER NOT NULL,
  content_sha TEXT,
  mode        INTEGER NOT NULL,
  mtime_ns    INTEGER NOT NULL,
  host_id     TEXT NOT NULL,
  ingested_at INTEGER NOT NULL,
  PRIMARY KEY (path, version)
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS fv_content_sha
  ON file_versions(content_sha)
  WHERE content_sha IS NOT NULL;

CREATE TABLE IF NOT EXISTS hosts (
  id        TEXT PRIMARY KEY,
  name      TEXT,
  last_seen INTEGER
) STRICT;

CREATE TABLE IF NOT EXISTS log_lines (
  path           TEXT NOT NULL,
  line_hash      TEXT NOT NULL,
  contents       BLOB NOT NULL,
  suggested_time INTEGER NOT NULL,
  first_seen     INTEGER NOT NULL,
  first_host     TEXT NOT NULL,
  PRIMARY KEY (path, line_hash)
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS log_lines_cursor
  ON log_lines(first_seen);

CREATE INDEX IF NOT EXISTS log_lines_path_time
  ON log_lines(path, suggested_time);

CREATE INDEX IF NOT EXISTS log_lines_path_first
  ON log_lines(path, first_seen, first_host);

CREATE TABLE IF NOT EXISTS path_facts (
  path       TEXT PRIMARY KEY,
  line_count INTEGER NOT NULL,
  facts      BLOB NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS log_paths (
  path       TEXT PRIMARY KEY,
  line_count INTEGER NOT NULL,
  last_time  INTEGER NOT NULL,
  total_size INTEGER NOT NULL,
  first_seen INTEGER NOT NULL,
  first_host TEXT NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TRIGGER IF NOT EXISTS log_paths_on_insert AFTER INSERT ON log_lines BEGIN
  INSERT INTO log_paths(path, line_count, last_time, total_size, first_seen, first_host)
  VALUES (NEW.path, 1, NEW.suggested_time, LENGTH(NEW.contents), NEW.first_seen, NEW.first_host)
  ON CONFLICT(path) DO UPDATE SET
    line_count = line_count + 1,
    last_time  = MAX(last_time, excluded.last_time),
    total_size = total_size + excluded.total_size,
    first_host = CASE WHEN excluded.first_seen < first_seen THEN excluded.first_host ELSE first_host END,
    first_seen = MIN(first_seen, excluded.first_seen);
END;

CREATE TABLE IF NOT EXISTS session_index (
  path              TEXT PRIMARY KEY,
  id                TEXT NOT NULL,
  session_id        TEXT NOT NULL,
  agent             TEXT NOT NULL,
  host              TEXT NOT NULL,
  user              TEXT NOT NULL,
  cwd               TEXT NOT NULL,
  project           TEXT NOT NULL,
  git_branch        TEXT NOT NULL,
  git_commit        TEXT NOT NULL,
  version           TEXT NOT NULL,
  models            TEXT NOT NULL,
  started_at        INTEGER NOT NULL,
  ended_at          INTEGER NOT NULL,
  turns             INTEGER NOT NULL,
  requests          INTEGER NOT NULL,
  tool_calls        INTEGER NOT NULL,
  errors            INTEGER NOT NULL,
  input_tokens      INTEGER NOT NULL,
  output_tokens     INTEGER NOT NULL,
  cache_read        INTEGER NOT NULL,
  cache_creation    INTEGER NOT NULL,
  cost_usd          REAL NOT NULL,
  cost_reported     INTEGER NOT NULL,
  parent_path       TEXT NOT NULL,
  parent_session_id TEXT NOT NULL,
  source            TEXT NOT NULL,
  title             TEXT NOT NULL,
  first_prompt      TEXT NOT NULL,
  line_count        INTEGER NOT NULL,
  cursor            INTEGER NOT NULL,
  state             BLOB NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS session_index_id ON session_index(id);
CREATE INDEX IF NOT EXISTS session_index_sid ON session_index(session_id);
CREATE INDEX IF NOT EXISTS session_index_parent ON session_index(parent_path);
CREATE INDEX IF NOT EXISTS session_index_started ON session_index(started_at);

CREATE TABLE IF NOT EXISTS session_writes (
  file_path    TEXT NOT NULL,
  session_path TEXT NOT NULL,
  ts           INTEGER NOT NULL,
  host         TEXT NOT NULL,
  tool         TEXT NOT NULL,
  PRIMARY KEY (file_path, ts, session_path)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS session_index_meta (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  version        INTEGER NOT NULL,
  gen            INTEGER NOT NULL,
  cursor         INTEGER NOT NULL,
  rebuild_active INTEGER NOT NULL,
  rebuild_head   INTEGER NOT NULL,
  rebuild_path   TEXT NOT NULL,
  updated_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS file_reviews (
  path       TEXT NOT NULL,
  version    INTEGER NOT NULL,
  decision   TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  reviewer   TEXT NOT NULL,
  note       TEXT NOT NULL,
  decided_at INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS file_reviews_path
  ON file_reviews(path, decided_at);

CREATE TABLE IF NOT EXISTS file_version_meta (
  path     TEXT NOT NULL,
  version  INTEGER NOT NULL,
  agent    TEXT NOT NULL,
  sessions TEXT NOT NULL,
  PRIMARY KEY (path, version)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS review_prechecks (
  path       TEXT NOT NULL,
  version    INTEGER NOT NULL,
  verdict    TEXT NOT NULL,
  detail     TEXT NOT NULL,
  checked_at INTEGER NOT NULL,
  PRIMARY KEY (path, version)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS review_meta (
  key   TEXT PRIMARY KEY,
  value INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

INSERT OR IGNORE INTO review_meta(key, value)
  VALUES ('gate_since', CAST((julianday('now') - 2440587.5) * 86400000000000 AS INTEGER));
