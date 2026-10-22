#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/multica-check.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_DIR/scripts" "$TEST_DIR/server" "$TEST_DIR/bin"
# Keep the production script's fixed service logs inside the fixture too.
sed "s|/tmp/multica-check-|$TEST_DIR/multica-check-|g" \
  "$SCRIPT_DIR/check.sh" >"$TEST_DIR/scripts/check.sh"
export CHECK_TEST_DIR="$TEST_DIR"

cat >"$TEST_DIR/.env" <<'ENV'
PORT=18080
FRONTEND_PORT=13000
ENV

# Override process operations as well as external tools: no real service is
# started, contacted, or killed, even in the ownership/cleanup cases.
cat >"$TEST_DIR/scripts/local-env.sh" <<'FAKE'
kill() {
  if [ "$1" = "$BACKEND_PID" ]; then
    echo backend >>"$CHECK_TEST_DIR/killed"
  elif [ "$1" = "$FRONTEND_PID" ]; then
    echo frontend >>"$CHECK_TEST_DIR/killed"
  else
    echo "unexpected PID: $1" >>"$CHECK_TEST_DIR/killed"
  fi
  return "${CLEANUP_STATUS:-0}"
}
wait() { return 0; }
FAKE
cat >"$TEST_DIR/scripts/ensure-postgres.sh" <<'FAKE'
exit "${PREREQUISITE_STATUS:-0}"
FAKE
printf 'exit 0\n' >"$TEST_DIR/scripts/test-go.test.sh"
printf 'exit 0\n' >"$TEST_DIR/scripts/test-go.sh"
cat >"$TEST_DIR/bin/pnpm" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CHECK_TEST_DIR/pnpm-calls"
if [ "$*" = "${FAIL_PHASE:-}" ]; then
  exit 23
fi
[ "$*" = "dev:web" ] && exec "$CHECK_TEST_DIR/bin/fake-server"
exit 0
FAKE
# Stands in for `next start` / `next dev`: records its PID like the real
# server owning the port, and lives until the fixture directory is removed.
cat >"$TEST_DIR/bin/fake-server" <<'FAKE'
#!/usr/bin/env bash
[ -n "${NEXT_EXIT:-}" ] && exit 1
echo $$ >"$CHECK_TEST_DIR/server.pid"
# Lost the bind race to a foreign listener: alive at first, then exits.
[ -n "${NEXT_LATE_EXIT:-}" ] && { sleep 1; exit 1; }
while [ -d "$CHECK_TEST_DIR/bin" ]; do sleep 0.1; done
FAKE
mkdir -p "$TEST_DIR/apps/web/node_modules/.bin"
cat >"$TEST_DIR/apps/web/node_modules/.bin/next" <<'FAKE'
#!/usr/bin/env bash
if [ "$1" = build ]; then
  printf 'next %s\n' "$*" >>"$CHECK_TEST_DIR/pnpm-calls"
  exit "${BUILD_STATUS:-0}"
fi
printf 'next %s\n' "$*" >>"$CHECK_TEST_DIR/pnpm-calls"
exec "$CHECK_TEST_DIR/bin/fake-server"
FAKE
# The listener's owner: the fake server, or a foreign PID outside its tree.
cat >"$TEST_DIR/bin/lsof" <<'FAKE'
#!/usr/bin/env bash
if [ -n "${NO_LISTENER_PID:-}" ]; then exit 0; fi
if [ -n "${FOREIGN_OWNER:-}" ]; then echo 1; else cat "$CHECK_TEST_DIR/server.pid" 2>/dev/null; fi
FAKE
cat >"$TEST_DIR/bin/go" <<'FAKE'
#!/usr/bin/env bash
exit 0
FAKE
cat >"$TEST_DIR/bin/curl" <<'FAKE'
#!/usr/bin/env bash
case "$*" in
  *18080*) service=backend ;;
  *13000*) service=frontend ;;
  *) exit 2 ;;
esac
if [ "$service" = frontend ]; then
  # `-sf` is the readiness probe; without `-f` curl reports any HTTP answer
  # (even 503) as success and only a refused connection as exit 7.
  case " $* " in
    *" -sf "*)
      if [ -n "${FOREIGN_FRONTEND:-}" ] && [ ! -f "$CHECK_TEST_DIR/frontend-probed" ]; then
        touch "$CHECK_TEST_DIR/frontend-probed"
        exit 22 # the foreign listener answers 503 first, then 200
      fi
      if [ -n "${NEXT_EXIT:-}" ]; then
        sleep 0.5 # the launched server has died; a foreign listener now answers 200
      else
        for _ in $(seq 100); do [ -s "$CHECK_TEST_DIR/server.pid" ] && break; sleep 0.1; done
      fi
      exit 0 ;;
    *) [ -n "${FOREIGN_FRONTEND:-}" ] && exit 0 || exit 7 ;;
  esac
fi
# First backend probe sees the configured existing service; later probes see
# the newly started stub as ready, so tests never sleep or access the network.
if [ ! -f "$CHECK_TEST_DIR/$service-probed" ]; then
  touch "$CHECK_TEST_DIR/$service-probed"
  case ":${START_SERVICES:-}:" in
    *":$service:"*) exit 1 ;;
  esac
fi
FAKE
chmod 755 "$TEST_DIR/bin/"* "$TEST_DIR/apps/web/node_modules/.bin/next"

fail() {
  echo "$label: $*" >&2
  cat "$TEST_DIR/output" >&2
  exit 1
}

# $1: label; $2: status; $3: services expected to be stopped; rest: env overrides.
run_case() {
  label=$1 expected_status=$2 expected_killed=$3
  shift 3
  rm -f "$TEST_DIR/"*-probed "$TEST_DIR/server.pid"
  : >"$TEST_DIR/killed"
  : >"$TEST_DIR/pnpm-calls"
  set +e
  (
    cd "$TEST_DIR"
    env -u BASH_ENV ENV_FILE=.env PATH="$TEST_DIR/bin:$PATH" \
      PREREQUISITE_STATUS=0 FAIL_PHASE= START_SERVICES= CLEANUP_STATUS=0 \
      FOREIGN_FRONTEND= FOREIGN_OWNER= NEXT_EXIT= NEXT_LATE_EXIT= NO_LISTENER_PID= BUILD_STATUS=0 E2E_WEB_MODE= \
      "$@" bash scripts/check.sh
  ) >"$TEST_DIR/output" 2>&1
  status=$?
  set -e
  [ "$status" -eq "$expected_status" ] || fail "status $status, want $expected_status"
  if [ "$expected_status" -eq 0 ]; then
    grep -q '^✓ All checks passed\.$' "$TEST_DIR/output" || fail "missing success footer"
    ! grep -q 'Checks FAILED' "$TEST_DIR/output" || fail "unexpected failure footer"
  else
    grep -q '^✗ Checks FAILED\.$' "$TEST_DIR/output" || fail "missing failure footer"
    ! grep -q 'All checks passed' "$TEST_DIR/output" || fail "misleading success footer"
  fi
  [ "$(cat "$TEST_DIR/killed")" = "$expected_killed" ] || fail "wrong cleanup ownership"
  echo "$label: PASS"
}

run_case 'prerequisite failure' 37 '' PREREQUISITE_STATUS=37
[ ! -s "$TEST_DIR/pnpm-calls" ] || fail 'continued after prerequisite failure'
run_case 'guarded typecheck failure' 1 '' FAIL_PHASE=typecheck
# R1: a foreign listener that answers 503 hides from `curl -sf`. It must be
# refused whatever its status, and never killed or adopted as the frontend.
run_case 'foreign frontend answering 503 is refused' 1 '' FOREIGN_FRONTEND=503
grep -q 'listens on :13000' "$TEST_DIR/output" || fail 'did not name the occupied port'
[ ! -s "$TEST_DIR/server.pid" ] || fail 'started a frontend next to a foreign listener'
run_case 'launched frontend that exits before ready fails' 1 '' NEXT_EXIT=1
grep -q 'exited' "$TEST_DIR/output" || fail 'did not report the exited frontend'
run_case 'ready listener owned by a foreign process fails' 1 frontend FOREIGN_OWNER=1
grep -q 'not owned' "$TEST_DIR/output" || fail 'did not report the foreign owner'
# A foreign listener that appears during the build wins the bind: the launched
# frontend is alive at first, then exits, and the probe answers from the
# foreign listener. With no tool able to name the listener's PID, ownership is
# unprovable, so the run fails instead of trusting that the process is alive.
run_case 'unprovable ownership after losing the bind race fails' 1 frontend NEXT_LATE_EXIT=1 NO_LISTENER_PID=1
grep -q 'cannot establish' "$TEST_DIR/output" || fail 'did not report the unprovable owner'
run_case 'unprovable ownership fails even when the frontend stays alive' 1 frontend NO_LISTENER_PID=1
run_case 'production build failure stops before E2E' 1 '' BUILD_STATUS=1
grep -q 'production build failed' "$TEST_DIR/output" || fail 'did not report the build failure'
! grep -q 'exec playwright test' "$TEST_DIR/pnpm-calls" || fail 'ran E2E after a failed build'
run_case 'success with an existing backend' 0 frontend
grep -q '^exec playwright test$' "$TEST_DIR/pnpm-calls" || fail 'did not reach E2E'
grep -q '^next build --webpack$' "$TEST_DIR/pnpm-calls" || fail 'did not build the production frontend'
grep -q '^next start --port 13000$' "$TEST_DIR/pnpm-calls" || fail 'did not start the production frontend'
! grep -q '^dev:web$' "$TEST_DIR/pnpm-calls" || fail 'used next dev by default'
run_case 'dev smoke mode starts next dev' 0 frontend E2E_WEB_MODE=dev
grep -q '^dev:web$' "$TEST_DIR/pnpm-calls" || fail 'did not start next dev'
! grep -q '^next build' "$TEST_DIR/pnpm-calls" || fail 'built in dev mode'
# `. .env` searches PATH before the working directory; a runner PATH entry
# that holds its own `.env` must not shadow the fixture's.
mkdir -p "$TEST_DIR/decoy"
printf 'UNRELATED=1\n' >"$TEST_DIR/decoy/.env"
run_case 'env file is not looked up on PATH' 0 frontend PATH="$TEST_DIR/decoy:$TEST_DIR/bin:$PATH"
run_case 'success with owned services' 0 'backend
frontend' START_SERVICES=backend
run_case 'E2E failure with owned backend' 1 'backend
frontend' START_SERVICES=backend FAIL_PHASE='exec playwright test'
run_case 'E2E failure with owned frontend' 1 frontend FAIL_PHASE='exec playwright test'
run_case 'cleanup failure preserves failed checks' 1 'backend
frontend' START_SERVICES=backend FAIL_PHASE='exec playwright test' CLEANUP_STATUS=1

echo 'check.test.sh: PASS'
