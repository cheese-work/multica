#!/usr/bin/env bash
# CHE-6563 (CHE-6210 A2c): fresh-install versus upgrade schema parity.
#   db "fresh":   all migrations of the tree, applied to an empty database.
#   db "upgrade": migrations of BASE_REF (its own migrate binary), then the tree's.
# Normalized `pg_dump --schema-only` of both must be identical; any difference fails.
# When the renumber map exists, the mapped down files then run in reverse on "upgrade"
# and the schema is compared with BASE_REF's; those differences are listed, not failed.
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
  sql "DROP DATABASE IF EXISTS parity_fresh" || true; sql "DROP DATABASE IF EXISTS parity_upgrade" || true
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
  # Mapped stems all sort after the last unmapped one, so --to that one undoes exactly the mapped set.
  mapped=$(node -p 'JSON.parse(require("fs").readFileSync(process.argv[1])).map(m=>m.to+".up.sql").join("\n")' "$MAP")
  last=$(cd server/migrations && ls -- *.up.sql | grep -vxF "$mapped" | LC_ALL=C sort | tail -1)
  last=${last%.up.sql}
  if (cd server && DATABASE_URL=$(url parity_upgrade) "$work/migrate" down --to "$last"); then
    dump parity_upgrade "$work/down.sql"
    diff -u "$work/base.sql" "$work/down.sql" > "$work/down.diff" && echo "down parity ok: mapped down files restore the $BASE_REF schema" \
      || { echo "::warning::mapped down files leave schema differences from $BASE_REF (listed, not failing; data loss is a separate decision)"; cat "$work/down.diff"; }
  else
    echo "::warning::down --to $last failed; schema differences after rollback are unknown"
  fi
fi
exit $status
