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
