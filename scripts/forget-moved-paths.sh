#!/usr/bin/env bash
# One-off, run by hand on the server host (root, next to a running cccp serve):
#   forget-moved-paths.sh DB HOST_ID FROM_PREFIX TO_PREFIX [--apply]
# Lists every log path under FROM_PREFIX whose first line came from HOST_ID and the path
# it was re-shipped to under TO_PREFIX (an agent that moved into a space of its own
# re-pushes its whole tree). Without --apply it only prints the plan. With --apply it
# deletes, in one transaction, the old copies whose every line is already in the new
# path and that carry no thumbs: their log_lines, log_paths, path_facts and
# session_index rows. file_versions, SAE records and session_writes are left alone.
set -euo pipefail

usage() {
  echo "usage: $0 DB HOST_ID FROM_PREFIX TO_PREFIX [--apply]" >&2
  exit 2
}
[ $# -eq 4 ] || [ $# -eq 5 ] || usage
db=$1 host=$2 from=$3 to=$4 apply=${5:-}
[ -z "$apply" ] || [ "$apply" = --apply ] || usage
[[ $host =~ ^[0-9a-f-]{36}$ ]] || usage
[[ $from =~ ^agent/[a-z0-9._-]+/$ && $to =~ ^agent/[a-z0-9._-]+/$ && $from != "$to" ]] || usage
[ -f "$db" ] || usage
end=${from%/}0

plan="
CREATE TEMP TABLE moved AS
  SELECT path AS old, '$to' || substr(path, length('$from') + 1) AS new, line_count
  FROM log_paths WHERE path >= '$from' AND path < '$end' AND first_host = '$host';
CREATE TEMP TABLE checked AS
  SELECT m.old, m.new, m.line_count,
    (SELECT COUNT(*) FROM log_lines o WHERE o.path = m.old AND NOT EXISTS
      (SELECT 1 FROM log_lines n WHERE n.path = m.new AND n.line_hash = o.line_hash)) AS missing,
    (SELECT COUNT(*) FROM feedback f WHERE f.path = m.old) AS votes
  FROM moved m;
"
report="SELECT CASE WHEN missing = 0 AND votes = 0 THEN 'forget' ELSE 'keep' END, old, new, line_count, missing, votes FROM checked ORDER BY old;"
apply_sql="
BEGIN IMMEDIATE;
CREATE TEMP TABLE gone AS SELECT old FROM checked WHERE missing = 0 AND votes = 0;
DELETE FROM log_lines WHERE path IN (SELECT old FROM gone);
DELETE FROM log_paths WHERE path IN (SELECT old FROM gone);
DELETE FROM path_facts WHERE path IN (SELECT old FROM gone);
DELETE FROM session_index WHERE path IN (SELECT old FROM gone);
SELECT 'forgot ' || COUNT(*) || ' paths' FROM gone;
COMMIT;
"

echo "action	old	new	lines	missing_in_new	votes"
if [ "$apply" = --apply ]; then
  sqlite3 -bail -cmd '.timeout 30000' -separator $'\t' "$db" "$plan $report $apply_sql"
else
  sqlite3 -bail -readonly -cmd '.timeout 30000' -separator $'\t' "$db" "$plan $report"
fi
