#!/usr/bin/env bash
set -euo pipefail

# CHE-655 / CHE-609 round-trip regression test: old-image -> expanded schema
# -> new-writes -> old-image, against REAL Postgres and REAL
# server/cmd/migrate + server/cmd/server Go binaries built from two real
# commits — never a stub or a same-binary-different-flag substitute.
#
# CHE-609's live C00 qualification exercised a NO-migration candidate
# (its two compared commits added no migrations over each other), so the
# expand/contract rollback guard (cutover.sh's `ledger_at_or_after` check,
# deploy-lib.sh) was exercised but a GENUINE schema expansion never was —
# Opus's own review condition for that issue ("the round-trip test must
# exist before a real rollback is attempted on C00") was not fully met.
# This closes that gap on X99, and doubles as a regression test for CHE-655
# defect 1 (cutover.sh's quiescence final-gate ran before the incumbent was
# drained): the sequence below drains the old image BEFORE the
# schema-expanding migration runs, exactly matching cutover.sh's fixed
# ordering.
#
# "old image" / "new image" are real, distinct binaries: "old" is built
# from the PARENT of the commit that introduced the CURRENT newest
# migration file, "new" is built from current HEAD. This is resolved
# dynamically (never a hardcoded historical SHA) so the test keeps
# exercising "whatever the newest migration is" indefinitely, rather than
# going stale the moment the next migration lands.
#
# The harness is slot-aware in the sense cutover.sh itself is: only one
# backend binary is ever bound to the shared port at a time, matching
# docker-compose.ab.yml's "never simultaneous serving" invariant.
#
# Requires Go and Docker; skips (not fails) if either is unavailable, same
# convention as test-migrate-supervised.sh.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

go_bin="${GO_BIN:-$(command -v go || true)}"
if [ -z "$go_bin" ] && [ -x /usr/local/go/bin/go ]; then
  go_bin=/usr/local/go/bin/go
fi
if [ -z "$go_bin" ]; then
  echo "SKIP: no Go toolchain found on PATH, GO_BIN, or /usr/local/go/bin/go — cannot build server/cmd/migrate or server/cmd/server" >&2
  exit 0
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "SKIP: no docker on PATH — cannot start a synthetic postgres" >&2
  exit 0
fi

work_dir="$(mktemp -d)"
old_worktree="$work_dir/old-worktree"
container="che655-rt-pg-$$"
pg_port=25710
backend_port=25810
server_pid=""

cleanup() {
  [ -n "$server_pid" ] && kill "$server_pid" >/dev/null 2>&1 || true
  [ -n "$server_pid" ] && wait "$server_pid" 2>/dev/null || true
  docker rm -f "$container" >/dev/null 2>&1 || true
  git worktree remove --force "$old_worktree" >/dev/null 2>&1 || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

# Resolve "newest migration" / "old image" dynamically — see header comment.
newest_migration="$(ls server/migrations | grep '\.up\.sql$' | sort | tail -1)"
introducing_commit="$(git log --format='%H' -1 -- "server/migrations/$newest_migration")"
if [ -z "$introducing_commit" ]; then
  echo "SKIP: could not resolve the commit that introduced $newest_migration" >&2
  exit 0
fi
old_ref="${introducing_commit}^"
if ! old_sha="$(git rev-parse --verify -q "$old_ref")"; then
  echo "SKIP: $introducing_commit has no parent commit — cannot build an old image to compare against" >&2
  exit 0
fi
echo "==> newest migration: $newest_migration (introduced by ${introducing_commit:0:12}); old image = ${old_sha:0:12}"

echo "==> building NEW (current HEAD) migrate + server binaries"
(cd server && "$go_bin" build -o "$work_dir/new_migrate" ./cmd/migrate)
(cd server && "$go_bin" build -o "$work_dir/new_server" ./cmd/server)

echo "==> building OLD (${old_sha:0:12}) migrate + server binaries in a scratch worktree"
git worktree add --detach "$old_worktree" "$old_ref" >/dev/null
(cd "$old_worktree/server" && "$go_bin" build -o "$work_dir/old_migrate" ./cmd/migrate)
(cd "$old_worktree/server" && "$go_bin" build -o "$work_dir/old_server" ./cmd/server)
# old_worktree itself (not just the binaries) must survive until this
# script's own cleanup trap: migrations.ResolveDir() (server/internal/
# migrations/migrations.go) walks up from the CURRENT WORKING DIRECTORY at
# RUN time looking for a "migrations"/"server/migrations" directory — it is
# never embedded into the binary at build time. Running old_migrate/
# old_server from root_dir (this script's own cwd, which has the FULL
# current migration set) would silently apply/see every migration up to
# HEAD regardless of which commit the binary was built from — exactly the
# failure mode this test exists to catch, so it must not reintroduce it via
# its own harness. old_migrate/old_server are therefore always invoked with
# cwd=$old_worktree below (see run_old()).

echo "==> starting throwaway postgres ($container)"
docker run -d --rm --name "$container" -p "127.0.0.1:${pg_port}:5432" \
  -e POSTGRES_USER=multica -e POSTGRES_PASSWORD=multica -e POSTGRES_DB=multica \
  postgres:16-alpine >/dev/null
pg_ready=""
for _ in $(seq 1 60); do
  if docker exec "$container" pg_isready -U multica -d multica >/dev/null 2>&1; then
    pg_ready=1
    break
  fi
  sleep 1
done
if [ -z "$pg_ready" ]; then
  echo "postgres never became ready in $container; container logs:" >&2
  docker logs "$container" 2>&1 | tail -40 >&2
  exit 1
fi

db_url="postgres://multica:multica@127.0.0.1:${pg_port}/multica?sslmode=disable"
jwt_secret="deadbeef00000000000000000000000000000000000000000000000000ff"

pass=0
fail=0
expect_true() {
  local name=$1
  if "${@:2}"; then
    echo "PASS: $name"
    pass=$((pass + 1))
  else
    echo "FAIL: $name"
    fail=$((fail + 1))
  fi
}

wait_ready_on_port() {
  local port=$1 timeout_s=$2 waited=0
  while [ "$waited" -lt "$timeout_s" ]; do
    curl --fail --silent --show-error "http://127.0.0.1:${port}/readyz" >/dev/null 2>&1 && return 0
    sleep 1
    waited=$((waited + 1))
  done
  return 1
}

# start_server launches $bin with cwd=$cwd (see the old_worktree comment
# above for why this matters: migrations.ResolveDir() resolves relative to
# the process's current working directory at run time, not build time).
start_server() {
  local bin=$1 logfile=$2 cwd=$3
  (
    cd "$cwd"
    exec env DATABASE_URL="$db_url" JWT_SECRET="$jwt_secret" PORT="$backend_port" \
      REDIS_URL="" MAINTENANCE_PORT="" METRICS_ADDR="" APP_ENV=test \
      "$bin" >"$logfile" 2>&1
  ) &
  server_pid=$!
}

run_old_migrate() {
  (cd "$old_worktree" && exec env DATABASE_URL="$db_url" "$work_dir/old_migrate" "$@")
}

run_new_migrate() {
  (cd "$root_dir" && exec env DATABASE_URL="$db_url" "$work_dir/new_migrate" "$@")
}

stop_server() {
  [ -z "$server_pid" ] && return 0
  kill "$server_pid" >/dev/null 2>&1 || true
  wait "$server_pid" 2>/dev/null || true
  server_pid=""
}

public_tables() {
  docker exec "$container" psql -U multica -d multica -At -c \
    "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY 1"
}

# Step 1: old image, baseline (pre-expansion) schema — mirrors cutover.sh's
# retained predecessor colour before any cutover has ever touched it.
echo "==> [1] old migrate up (baseline schema, without $newest_migration)"
run_old_migrate up
tables_before="$(public_tables)"

echo "==> [1] starting OLD server against baseline schema"
start_server "$work_dir/old_server" "$work_dir/old-server-baseline.log" "$old_worktree"
expect_true "old server becomes ready on baseline schema" wait_ready_on_port "$backend_port" 30
stop_server

# Step 2: drain old, THEN expand the schema — this ordering (drain before
# the schema-changing migration runs) is exactly CHE-655 defect 1's fix in
# cutover.sh; doing it in the other order is the bug that fix corrects.
echo "==> [2] new migrate up (expand: applies $newest_migration)"
run_new_migrate up
tables_after="$(public_tables)"
new_tables="$(comm -13 <(printf '%s\n' "$tables_before") <(printf '%s\n' "$tables_after"))"

# Step 3: new image against the expanded schema — health-check, then
# perform a genuine "new write": a row only the new schema (and the code
# that shipped with it) can create. The target table is discovered
# generically from the schema diff above, never hardcoded, so this test
# keeps exercising "whatever migration is newest" as the repo grows.
echo "==> [3] starting NEW server against expanded schema"
start_server "$work_dir/new_server" "$work_dir/new-server.log" "$root_dir"
expect_true "new server becomes ready on expanded schema" wait_ready_on_port "$backend_port" 30

if [ -z "$new_tables" ]; then
  echo "NOTE: $newest_migration created no new table (e.g. an index-only migration) — skipping the new-write step; round-trip health-check coverage below still applies"
else
  echo "==> [3] new-writes: inserting a synthetic row into: $(printf '%s ' $new_tables)"
  for table in $new_tables; do
    docker exec "$container" psql -U multica -d multica -v ON_ERROR_STOP=1 -c "
      DO \$\$
      DECLARE
        r record;
        cols text := '';
        vals text := '';
        v text;
      BEGIN
        FOR r IN
          SELECT column_name, data_type
          FROM information_schema.columns
          WHERE table_schema = 'public' AND table_name = '${table}'
            AND is_nullable = 'NO' AND column_default IS NULL
        LOOP
          v := CASE r.data_type
            WHEN 'uuid' THEN 'gen_random_uuid()'
            WHEN 'text' THEN quote_literal('che655-round-trip')
            WHEN 'character varying' THEN quote_literal('che655-round-trip')
            WHEN 'integer' THEN '0'
            WHEN 'bigint' THEN '0'
            WHEN 'smallint' THEN '0'
            WHEN 'boolean' THEN 'true'
            WHEN 'timestamp with time zone' THEN 'now()'
            WHEN 'timestamp without time zone' THEN 'now()'
            WHEN 'date' THEN 'now()'
            WHEN 'jsonb' THEN quote_literal('{}')
            WHEN 'json' THEN quote_literal('{}')
            WHEN 'ARRAY' THEN quote_literal('{}')
            ELSE NULL
          END;
          IF v IS NULL THEN
            RAISE EXCEPTION 'unsupported NOT NULL column type % on %.% -- extend this test''s type map', r.data_type, '${table}', r.column_name;
          END IF;
          cols := cols || quote_ident(r.column_name) || ',';
          vals := vals || v || ',';
        END LOOP;
        IF cols = '' THEN
          EXECUTE format('INSERT INTO %I DEFAULT VALUES', '${table}');
        ELSE
          EXECUTE format('INSERT INTO %I (%s) VALUES (%s)', '${table}', left(cols, length(cols)-1), left(vals, length(vals)-1));
        END IF;
      END \$\$;
    " >/dev/null
    row_count="$(docker exec "$container" psql -U multica -d multica -At -c "SELECT count(*) FROM ${table}")"
    expect_true "new-write landed in ${table} (new server, new schema)" [ "$row_count" -ge 1 ]
  done
fi
stop_server

# Step 4: the round-trip assertion — restart the OLD image against the NOW
# EXPANDED schema, with the new writes from step 3 already present. This is
# exactly cutover.sh's rollback path: the retained predecessor is REQUIRED
# to tolerate the expanded (post-migration) schema, including rows only new
# code ever wrote (deploy-lib.sh's ledger_at_or_after / cutover.sh's
# rollback comment on why this is the normal, expected case under
# expand/contract, not an unsafe one).
echo "==> [4] restarting OLD server against the expanded schema (round trip)"
start_server "$work_dir/old_server" "$work_dir/old-server-post-expand.log" "$old_worktree"
expect_true "old server becomes ready again on the EXPANDED schema (round-trip)" wait_ready_on_port "$backend_port" 30
if [ -n "$new_tables" ]; then
  health_after_roundtrip="$(curl --fail --silent --show-error "http://127.0.0.1:${backend_port}/health" || echo "")"
  expect_true "old server /health still reports ok after round trip" [ -n "$health_after_roundtrip" ]
fi
stop_server

if grep -qiE 'panic:' "$work_dir"/old-server-*.log "$work_dir"/new-server.log 2>/dev/null; then
  echo "FAIL: a server binary panicked during the round trip — see logs in $work_dir before this script's EXIT trap cleans it up"
  grep -iE 'panic:' "$work_dir"/old-server-*.log "$work_dir"/new-server.log 2>/dev/null || true
  fail=$((fail + 1))
fi

echo
echo "==> $pass passed, $fail failed"
[ "$fail" -eq 0 ]
