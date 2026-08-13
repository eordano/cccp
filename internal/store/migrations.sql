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
