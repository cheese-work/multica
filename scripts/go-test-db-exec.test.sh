#!/usr/bin/env bash
# Needs DATABASE_URL pointing at a migrated, otherwise idle template database
# (the backend-tests job already has one). REDIS_TEST_URL is optional.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WRAPPER="$SCRIPT_DIR/go-test-db-exec.sh"
: "${DATABASE_URL:?DATABASE_URL must name the migrated template database}"

TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/multica-db-exec.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT

base=${DATABASE_URL%%\?*}
query=${DATABASE_URL#"$base"}
admin="${base%/*}/postgres$query"
template_db=${base##*/}

fail() { echo "go-test-db-exec.test.sh: $*" >&2; exit 1; }
db_exists() { [ "$(psql "$admin" -Atc "SELECT count(*) FROM pg_database WHERE datname = '$1'")" = 1 ]; }

# Child records its environment, writes into the database it was given, and exits 7.
cat >"$TEST_DIR/child.sh" <<'CHILD'
#!/usr/bin/env bash
echo "$DATABASE_URL" >>"$CHILD_OUT"
echo "${REDIS_TEST_URL:-}" >>"$CHILD_OUT"
psql "$DATABASE_URL" -Atc "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'" >>"$CHILD_OUT"
psql "$DATABASE_URL" -qc "CREATE TABLE per_package_marker (id int)"
exit 7
CHILD
chmod 755 "$TEST_DIR/child.sh"
export CHILD_OUT="$TEST_DIR/out"

# Off by default: the child sees the original URL untouched.
: >"$CHILD_OUT"
"$WRAPPER" "$TEST_DIR/child.sh" || true
[ "$(sed -n 1p "$CHILD_OUT")" = "$DATABASE_URL" ] || fail "passthrough changed DATABASE_URL"
psql "$DATABASE_URL" -qc "DROP TABLE per_package_marker"

# On: own database named *_test, cloned from the template, dropped afterwards.
: >"$CHILD_OUT"
status=0
MULTICA_TEST_DB_PER_PACKAGE=1 "$WRAPPER" "$TEST_DIR/child.sh" || status=$?
[ "$status" = 7 ] || fail "exit status $status, want 7"
child_url=$(sed -n 1p "$CHILD_OUT")
child_base=${child_url%%\?*}
child_db=${child_base##*/}
[ "$child_db" != "$template_db" ] || fail "child reused the template database"
case "$child_db" in *_test) ;; *) fail "database $child_db must end in _test" ;; esac
[ "$(sed -n 3p "$CHILD_OUT")" -gt 0 ] || fail "clone has no tables from the template"
if db_exists "$child_db"; then fail "database $child_db was not dropped"; fi
if psql "$DATABASE_URL" -Atc "SELECT to_regclass('per_package_marker')" | grep -q per_package; then
  fail "child write leaked into the template"
fi

# Two runs never share a database.
: >"$CHILD_OUT"
MULTICA_TEST_DB_PER_PACKAGE=1 "$WRAPPER" "$TEST_DIR/child.sh" || true
MULTICA_TEST_DB_PER_PACKAGE=1 "$WRAPPER" "$TEST_DIR/child.sh" || true
[ "$(sed -n '1p;4p' "$CHILD_OUT" | sort -u | wc -l)" = 2 ] || fail "two runs shared a database"

# Redis: concurrent runs get distinct DB indexes.
if [ -n "${REDIS_TEST_URL:-}" ]; then
  cat >"$TEST_DIR/slow.sh" <<'SLOW'
#!/usr/bin/env bash
echo "$REDIS_TEST_URL" >>"$CHILD_OUT"
sleep 2
SLOW
  chmod 755 "$TEST_DIR/slow.sh"
  : >"$CHILD_OUT"
  for _ in 1 2 3 4; do MULTICA_TEST_DB_PER_PACKAGE=1 "$WRAPPER" "$TEST_DIR/slow.sh" & done
  wait
  [ "$(sort -u "$CHILD_OUT" | wc -l)" = 4 ] || fail "concurrent runs shared a Redis DB: $(sort "$CHILD_OUT" | tr '\n' ' ')"
fi

echo "go-test-db-exec.test.sh: PASS"
