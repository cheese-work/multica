#!/usr/bin/env bash
set -euo pipefail

# Refusal and isolation tests for rehearse-cutover-x99.sh (CHE-7614 N6a).
# No real Docker: a stub `docker` on PATH records every call, so these prove
# what the runner refuses, what it would touch and what it cleans up. The real
# drills (real images, real Postgres, real cutover.sh) run on X99 by hand; see
# deploy/cd/README.md, "X99 cutover rehearsal".

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
runner="$root_dir/deploy/cd/rehearse-cutover-x99.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin" "$work/base"
cat >"$work/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$DOCKER_LOG"
case "${1:-}" in build | buildx) exit 1 ;; esac
exit 0
STUB
chmod +x "$work/bin/docker"
export DOCKER_LOG="$work/docker.log"
export REHEARSE_BASE="$work/base"
# Default `ss` shows no listener; the router-port test swaps in a busy one.
printf '#!/usr/bin/env bash\nexit 0\n' >"$work/bin/ss"
chmod +x "$work/bin/ss"
export PATH="$work/bin:$PATH"

fails=0
fail() { echo "FAIL: $*" >&2; fails=$((fails + 1)); }

# run_runner <name> <args...>: output in $work/<name>.out, exit code in $rc.
run_runner() {
  local name=$1
  shift
  : >"$DOCKER_LOG"
  rc=0
  bash "$runner" "$@" >"$work/$name.out" 2>&1 || rc=$?
}

# expect_refusal <name> <message fragment> <args...> (env set by the caller)
expect_refusal() {
  local name=$1 fragment=$2
  shift 2
  run_runner "$name" "$@"
  [ "$rc" = 2 ] || fail "$name: exit $rc, want 2"
  grep -qF -- "$fragment" "$work/$name.out" || fail "$name: output lacks '$fragment'"
  [ ! -s "$DOCKER_LOG" ] || fail "$name: docker was called before the refusal: $(head -1 "$DOCKER_LOG")"
  [ -z "$(ls -A "$work/base")" ] || fail "$name: left files under the base dir"
}

[ -f "$runner" ] || { echo "FAIL: $runner is missing" >&2; exit 1; }

# --- refusals (before any docker call, before any directory) ---
C00_COMPOSE_DIR=/srv/c00 expect_refusal c00-var "C00_COMPOSE_DIR" --run-id t1
C00_ANYTHING=x expect_refusal c00-any "C00_ANYTHING" --run-id t1
GHCR_PULL_TOKEN=secret-token expect_refusal ghcr "GHCR_PULL_TOKEN" --run-id t1
grep -q secret-token "$work/ghcr.out" && fail "ghcr: the token value was printed"
CUTOVER_DATABASE_URL='postgres://u:p@db.example:5432/x' expect_refusal db-url "own Postgres service" --run-id t1
DATABASE_URL='postgres://u:p@127.0.0.1:5432/x' expect_refusal db-url2 "own Postgres service" --run-id t1
n=0
for bad in 'UPPER' '../x' 'a b' '-lead' '' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; do
  n=$((n + 1))
  expect_refusal "runid-$n" "invalid --run-id" --run-id "$bad"
done
expect_refusal fault "unknown --fault" --run-id t1 --fault other
expect_refusal then "unknown --then" --run-id t1 --then other
expect_refusal fault-then "--then rollback needs a completed cutover" --run-id t1 --fault readyz --then rollback
expect_refusal unknown-arg "unknown argument" --nope

mkdir -p "$work/base/taken"
run_runner exists --run-id taken
[ "$rc" = 2 ] && grep -q "already exists" "$work/exists.out" || fail "existing run dir not refused (exit $rc)"
rmdir "$work/base/taken"

cat >"$work/bin/ss" <<'STUB'
#!/usr/bin/env bash
echo 'LISTEN 0 511 127.0.0.1:8081 0.0.0.0:*'
STUB
chmod +x "$work/bin/ss"
expect_refusal router-port "router port 8081" --run-id t1
printf '#!/usr/bin/env bash\nexit 0\n' >"$work/bin/ss"

# --- isolation: the plan names only this run's resources, no docker call ---
run_runner plan --run-id plan1 --dry-run --fault readyz
[ "$rc" = 0 ] || fail "dry-run exit $rc"
[ ! -s "$DOCKER_LOG" ] || fail "dry-run called docker"
[ -z "$(ls -A "$work/base")" ] || fail "dry-run created files"
grep -qx "project=multica-rehearsal-plan1" "$work/plan.out" || fail "plan lacks the project name"
for var in compose_dir state_dir router_state_dir; do
  grep -q "^$var=$work/base/plan1/" "$work/plan.out" || fail "plan: $var is not under the run dir"
done
grep -q '^DRILL_FAULT_READYZ=1$' "$work/plan.out" || fail "plan lacks the fault switch"
grep -q 'org.multica.rehearsal.role=candidate' "$work/plan.out" || fail "plan lacks the candidate role label"
grep -q 'org.multica.rehearsal.role=previous' "$work/plan.out" || fail "plan lacks the previous role label"
run_runner plan-default --dry-run
grep -q '^project=multica-rehearsal-[a-z0-9-]\+$' "$work/plan-default.out" || fail "default run id is not a valid name"

# --- cleanup: a failed run removes only its own artifacts, records df twice ---
run_runner cleanup --run-id clean1
[ "$rc" != 0 ] || fail "a failing build must fail the run"
[ "$(grep -c '^df -B1 before: ' "$work/cleanup.out")" = 1 ] || fail "no df -B1 before record"
[ "$(grep -c '^df -B1 after: ' "$work/cleanup.out")" = 1 ] || fail "no df -B1 after record"
[ ! -e "$work/base/clean1" ] || fail "the run dir survived cleanup"
grep -q 'compose -p multica-rehearsal-clean1 down' "$DOCKER_LOG" || fail "compose project not taken down by name"
if grep -E '^(rm|image rm|rmi|volume rm|network rm|compose)' "$DOCKER_LOG" | grep -v 'multica-rehearsal-clean1\|org.multica.rehearsal.run=clean1' | grep -q .; then
  fail "a cleanup call is not scoped to the run: $(grep -E '^(rm|image rm|rmi|volume rm|network rm|compose)' "$DOCKER_LOG" | grep -v 'multica-rehearsal-clean1\|org.multica.rehearsal.run=clean1' | head -1)"
fi
grep -qE 'prune|system|builder' "$DOCKER_LOG" && fail "cleanup made a broad docker call"

# --- fixture: one marked contract migration that sorts after every real one ---
fixture_dir="$root_dir/deploy/cd/fixtures/contract"
up=("$fixture_dir"/*.up.sql)
[ "${#up[@]}" = 1 ] && [ -f "${up[0]}" ] || fail "want exactly one contract fixture migration"
grep -q '^-- rehearsal:contract$' "${up[0]}" || fail "fixture is not marked '-- rehearsal:contract'"
version="$(basename "${up[0]}" .up.sql)"
[ -f "$fixture_dir/$version.down.sql" ] || fail "fixture has no down file"
newest="$(ls "$root_dir/server/migrations" | grep '\.up\.sql$' | sed 's/\.up\.sql$//' | LC_ALL=C sort | tail -1)"
[ "$(printf '%s\n%s\n' "$newest" "$version" | LC_ALL=C sort | tail -1)" = "$version" ] || fail "fixture $version does not sort after $newest"

[ "$fails" = 0 ] || { echo "$fails failure(s)" >&2; exit 1; }
echo "rehearse-cutover-x99 tests passed"
