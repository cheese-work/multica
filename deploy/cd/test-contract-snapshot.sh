#!/usr/bin/env bash
set -euo pipefail

# contract-snapshot.mjs and the COPY snapshot format (CHE-7486 N5a).
# Part 1 needs only node: the evidence-file helpers. Part 2 runs the same
# `psql -tA -c "COPY <t> TO STDOUT"` the controller runs, against a
# throwaway postgres container, and skips without Docker (never faked).

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
helper="$root_dir/deploy/cd/contract-snapshot.mjs"
work_dir="$(mktemp -d)"
container="n5a-copy-test-pg-$$"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# --- Part 1: helper files -------------------------------------------------
printf 'a\tb\n1\t2\n' >"$work_dir/t1.copy"
node "$helper" entry --version v1 --table t1 --rows 2 --copy "$work_dir/t1.copy" --columns-file /dev/stdin <<<$'a\nb' >"$work_dir/entry.json"
node "$helper" manifest --out "$work_dir/manifest.json" --attempt-id 0123456789abcdef0123456789abcdef \
  --source-sha deadbeef --taken-at 2026-11-28T00:00:00Z --ledger-latest v0 --ledger-rows 3 <"$work_dir/entry.json"
[ "$(stat -c %a "$work_dir/manifest.json")" = 600 ] || fail "manifest mode is not 0600"
node "$helper" verify --manifest "$work_dir/manifest.json" || fail "a good copy did not verify"
if node "$helper" manifest --out "$work_dir/manifest.json" --attempt-id x --source-sha x --taken-at x --ledger-latest x --ledger-rows 1 <"$work_dir/entry.json" 2>/dev/null; then
  fail "manifest overwrote an existing file"
fi
printf 'extra\n' >>"$work_dir/t1.copy"
if node "$helper" verify --manifest "$work_dir/manifest.json" 2>"$work_dir/err"; then fail "a changed copy verified"; fi
grep -q "lines" "$work_dir/err" || fail "line-count mismatch not reported: $(cat "$work_dir/err")"
printf 'a\tb\n1\t3\n' >"$work_dir/t1.copy"
if node "$helper" verify --manifest "$work_dir/manifest.json" 2>"$work_dir/err"; then fail "a same-length changed copy verified"; fi
grep -q "SHA-256" "$work_dir/err" || fail "SHA-256 mismatch not reported: $(cat "$work_dir/err")"
echo "helper file checks passed"

# --- Part 2: COPY against a real postgres ---------------------------------
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "SKIP: Docker is not available; the COPY round-trip was not run" >&2
  exit 0
fi
docker run -d --name "$container" -e POSTGRES_PASSWORD=synthetic-only -e POSTGRES_USER=multica -e POSTGRES_DB=multica postgres:16-alpine >/dev/null
# The image starts a temporary server for its init scripts, then restarts:
# ready means the second "ready to accept connections".
for _ in $(seq 1 60); do
  [ "$(docker logs "$container" 2>&1 | grep -c 'ready to accept connections')" -ge 2 ] && break
  sleep 1
done
psql_c() { docker exec -i "$container" psql -U multica -d multica -tA -v ON_ERROR_STOP=1 -c "$1" </dev/null; }

psql_c "CREATE TABLE n5a_rows (id int primary key, body text, note text)" >/dev/null
# Tabs, newlines, carriage returns, backslashes, quotes, NULL, empty string and
# non-ASCII text: every one must stay on its own single COPY line.
psql_c "INSERT INTO n5a_rows VALUES
  (1, E'tab\\there', 'x'),
  (2, E'line1\\nline2\\r\\nline3', NULL),
  (3, E'back\\\\slash \\\\N', ''),
  (4, 'caf' || chr(233) || ' ' || chr(26085) || chr(26412) || ' ' || chr(128640), 'quote''s'),
  (5, NULL, NULL)" >/dev/null
rows="$(psql_c "SELECT count(*) FROM n5a_rows")"
psql_c "COPY n5a_rows TO STDOUT" >"$work_dir/n5a_rows.copy"
lines="$(wc -l <"$work_dir/n5a_rows.copy" | tr -d ' ')"
[ "$rows" = 5 ] && [ "$lines" = "$rows" ] || fail "COPY has $lines lines for $rows rows"
grep -q $'^1\ttab\\\\there\tx$' "$work_dir/n5a_rows.copy" || fail "a tab was not escaped"
grep -q $'^3\tback\\\\\\\\slash \\\\\\\\N\t$' "$work_dir/n5a_rows.copy" || fail "backslashes were not escaped"
grep -q $'^5\t\\\\N\t\\\\N$' "$work_dir/n5a_rows.copy" || fail "NULLs were not written as \\N"
grep -q 'café' "$work_dir/n5a_rows.copy" || fail "non-ASCII text was changed"

# Restoring the file into a copy of the table gives the same rows.
psql_c "CREATE TABLE n5a_rows_back (LIKE n5a_rows)" >/dev/null
docker exec -i "$container" psql -U multica -d multica -v ON_ERROR_STOP=1 -c "COPY n5a_rows_back FROM STDIN" <"$work_dir/n5a_rows.copy" >/dev/null
same="$(psql_c "SELECT count(*) FROM (SELECT * FROM n5a_rows EXCEPT SELECT * FROM n5a_rows_back) a")"
back="$(psql_c "SELECT count(*) FROM n5a_rows_back")"
[ "$same" = 0 ] && [ "$back" = 5 ] || fail "COPY round trip lost rows (missing $same, back $back)"

# The helper's manifest for that real copy verifies, and a flipped byte does not.
psql_c "SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'n5a_rows' ORDER BY ordinal_position" >"$work_dir/cols"
mkdir "$work_dir/snap"
cp "$work_dir/n5a_rows.copy" "$work_dir/snap/n5a_rows.copy"
node "$helper" entry --version v1 --table n5a_rows --rows "$rows" --copy "$work_dir/snap/n5a_rows.copy" --columns-file "$work_dir/cols" |
  node "$helper" manifest --out "$work_dir/snap/manifest.json" --attempt-id 0123456789abcdef0123456789abcdef --source-sha deadbeef --taken-at 2026-11-28T00:00:00Z --ledger-latest v0 --ledger-rows 1
grep -q '"id"' "$work_dir/snap/manifest.json" && grep -q '"body"' "$work_dir/snap/manifest.json" || fail "manifest has no columns"
node "$helper" verify --manifest "$work_dir/snap/manifest.json" || fail "the real copy did not verify"
printf 'X' | dd of="$work_dir/snap/n5a_rows.copy" bs=1 seek=0 conv=notrunc 2>/dev/null
if node "$helper" verify --manifest "$work_dir/snap/manifest.json" 2>/dev/null; then fail "a flipped byte verified"; fi
echo "contract COPY round-trip passed"
