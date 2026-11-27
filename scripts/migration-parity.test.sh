#!/usr/bin/env bash
# CHE-10205: regression test for the down-parity step of scripts/migration-parity.sh.
# BASE_REF (HEAD) already holds the mapped 99zz stems; the "PR" adds one migration.
#   good: the new migration's down file undoes its up file -> no ::warning::, exit 0.
#   leak: the new migration's down file leaves the table behind -> a ::warning:: that
#         names the leak (the check must still catch real down-file schema leaks).
# Needs: go, git, node and a Postgres via PG* env (see migration-parity.sh).
# Usage: scripts/migration-parity.test.sh   (run from inside the repo)
set -euo pipefail
repo=$(git rev-parse --show-toplevel)
script="$repo/scripts/migration-parity.sh"
work=$(mktemp -d)
cleanup() { git -C "$repo" worktree remove --force "$work/tree" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

base=$(git -C "$repo" rev-parse HEAD)
# The renumber map must exist for the down step to run at all.
[ -f "$repo/server/cmd/migrate/renumber-map.json" ] || fail "renumber-map.json missing: the down step would not run"
grep -q '"99zz01_' "$repo/server/cmd/migrate/renumber-map.json" || fail "fixture assumes main already holds the mapped 99zz stems"

# run_case <name> <down sql> -> writes $work/<name>.out, echoes the exit code
run_case() {
  local name=$1 down=$2 code=0
  git -C "$repo" worktree add --detach "$work/tree" "$base" >/dev/null 2>&1
  printf 'CREATE TABLE parity_fixture (id int);\n' > "$work/tree/server/migrations/99j_parity_fixture.up.sql"
  printf '%s\n' "$down" > "$work/tree/server/migrations/99j_parity_fixture.down.sql"
  git -C "$work/tree" add -A
  git -C "$work/tree" -c user.name=t -c user.email=t@example.com commit -qm "add 99j_parity_fixture"
  # Run the script under test from inside the fixture tree (it resolves its root from cwd).
  (cd "$work/tree" && bash "$script" "$base") > "$work/$name.out" 2>&1 || code=$?
  git -C "$repo" worktree remove --force "$work/tree"
  echo "$code"
}

code=$(run_case good 'DROP TABLE parity_fixture;')
[ "$code" = 0 ] || { cat "$work/good.out"; fail "good: script exited $code"; }
if grep -q '::warning::' "$work/good.out"; then cat "$work/good.out"; fail "good: expected 0 warnings"; fi
grep -q 'down parity ok' "$work/good.out" || { cat "$work/good.out"; fail "good: down parity was not checked"; }

code=$(run_case leak 'SELECT 1;')
[ "$code" = 0 ] || { cat "$work/leak.out"; fail "leak: script exited $code (down leaks warn, never fail)"; }
grep -q '::warning::.*down files leave schema differences' "$work/leak.out" || { cat "$work/leak.out"; fail "leak: expected the down-leak warning"; }
grep -q 'parity_fixture' "$work/leak.out" || { cat "$work/leak.out"; fail "leak: diff should name the leaked table"; }

echo "migration-parity.test.sh: ok"
