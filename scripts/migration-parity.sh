#!/usr/bin/env bash
# CHE-6563 (CHE-6210 A2c): fresh-install versus upgrade schema parity.
#   db "fresh":   all migrations of the tree, applied to an empty database.
#   db "upgrade": migrations of BASE_REF (its own migrate binary), then the tree's.
# Normalized `pg_dump --schema-only` of both must be identical; any difference fails.
# When the renumber map exists, the down files of the stems the tree adds over BASE_REF run in
# reverse on "upgrade"; the same rollback runs on a third db holding BASE_REF, and the two
# schemas are compared (CHE-10205); those differences are listed, not failed.
# Needs: go, git, a Postgres reachable via PG* env. PG_RUN prefixes psql/pg_dump
# (default: the pgvector image over host networking; PG_RUN="" uses native clients).
# Usage: scripts/migration-parity.sh <BASE_REF>
set -euo pipefail
BASE_REF=${1:?usage: migration-parity.sh <BASE_REF>}
cd "$(git rev-parse --show-toplevel)"
export PGHOST=${PGHOST:-localhost} PGPORT=${PGPORT:-5432} PGUSER=${PGUSER:-multica} PGPASSWORD=${PGPASSWORD:-multica}
PG_RUN=${PG_RUN-docker run --rm --network host -i -e PGHOST -e PGPORT -e PGUSER -e PGPASSWORD ${PG_IMAGE:-pgvector/pgvector:pg17}}
MAP=server/cmd/migrate/renumber-map.json
work=$(mktemp -d)
url() { echo "postgres://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/$1?sslmode=disable"; }
sql() { $PG_RUN psql -v ON_ERROR_STOP=1 -d postgres -qc "$1"; }
dump() { $PG_RUN pg_dump --schema-only --no-owner --no-privileges -d "$1" | grep -Ev '^(--|\\(un)?restrict )' | cat -s > "$2"; }
cleanup() {
  git worktree remove --force "$work/base" 2>/dev/null || true
  for db in parity_fresh parity_upgrade parity_basedown; do sql "DROP DATABASE IF EXISTS $db" || true; done
  rm -rf "$work"
}
trap cleanup EXIT
for db in parity_fresh parity_upgrade; do sql "DROP DATABASE IF EXISTS $db"; sql "CREATE DATABASE $db"; done

git worktree add --detach "$work/base" "$BASE_REF" >/dev/null
(cd "$work/base/server" && go build -o "$work/migrate-base" ./cmd/migrate)
(cd server && go build -o "$work/migrate" ./cmd/migrate)
(cd "$work/base/server" && DATABASE_URL=$(url parity_upgrade) "$work/migrate-base" up)
dump parity_upgrade "$work/base.sql"
(cd server && DATABASE_URL=$(url parity_fresh) "$work/migrate" up)
(cd server && DATABASE_URL=$(url parity_upgrade) "$work/migrate" up)
dump parity_fresh "$work/fresh.sql"; dump parity_upgrade "$work/upgrade.sql"

status=0
if ! diff -u "$work/fresh.sql" "$work/upgrade.sql"; then
  echo "::error::fresh install and upgrade from $BASE_REF produce different schemas (diff above: - fresh, + upgrade)"
  status=1
else
  echo "parity ok: fresh install equals upgrade from $BASE_REF"
fi

if [ -f "$MAP" ]; then
  # Compare like with like: the stems this tree adds over BASE_REF (the PR's own migrations, plus the
  # mapped set when BASE_REF predates the map) are rolled back by their down files on "upgrade"; the same
  # rollback runs on a copy of BASE_REF's schema, so mapped stems BASE_REF already holds cancel out.
  stems() { (cd "$1" && ls -- *.up.sql | sed 's/\.up\.sql$//' | LC_ALL=C sort); }
  stems "$work/base/server/migrations" > "$work/base.stems"
  stems server/migrations > "$work/tree.stems"
  first=$(LC_ALL=C comm -13 "$work/base.stems" "$work/tree.stems" | head -1)
  if [ -z "$first" ]; then
    echo "down parity skipped: the tree adds no migration over $BASE_REF"
  else
    # Newest tree stem before the first added one; everything after it is rolled back on both databases.
    last=$(LC_ALL=C awk -v f="$first" '$0 < f' "$work/tree.stems" | tail -1)
    sql "DROP DATABASE IF EXISTS parity_basedown"; sql "CREATE DATABASE parity_basedown"
    (cd "$work/base/server" && DATABASE_URL=$(url parity_basedown) "$work/migrate-base" up >/dev/null)
    if [ -n "$last" ] \
      && (cd server && DATABASE_URL=$(url parity_basedown) "$work/migrate" down --to "$last" >/dev/null) \
      && (cd server && DATABASE_URL=$(url parity_upgrade) "$work/migrate" down --to "$last"); then
      dump parity_basedown "$work/base-down.sql"; dump parity_upgrade "$work/down.sql"
      diff -u "$work/base-down.sql" "$work/down.sql" > "$work/down.diff" && echo "down parity ok: down files of the stems added over $BASE_REF restore its schema" \
        || { echo "::warning::down files leave schema differences from $BASE_REF (listed, not failing; data loss is a separate decision)"; cat "$work/down.diff"; }
    else
      echo "::warning::down --to ${last:-<none>} failed; schema differences after rollback are unknown"
    fi
  fi
fi
exit $status
