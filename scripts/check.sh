#!/usr/bin/env bash
set -euo pipefail

# ==========================================================================
# Full verification pipeline: typecheck → unit tests → Go tests → E2E
# Usage: bash scripts/check.sh
# ==========================================================================

ENV_FILE="${ENV_FILE:-.env}"
if [ ! -f "$ENV_FILE" ]; then
  echo "Missing env file: $ENV_FILE"
  echo "Create .env from .env.example, or run 'make worktree-env' and use .env.worktree."
  exit 1
fi

# A bare name would make `.` search PATH before the working directory.
case "$ENV_FILE" in
  */*) ENV_SOURCE="$ENV_FILE" ;;
  *) ENV_SOURCE="./$ENV_FILE" ;;
esac

set -a
# shellcheck disable=SC1090
. "$ENV_SOURCE"
set +a

# shellcheck disable=SC1091
. scripts/local-env.sh

BACKEND_PID=""
FRONTEND_PID=""
STARTED_BACKEND=false
STARTED_FRONTEND=false
EXIT_CODE=0

# --------------------------------------------------------------------------
# Cleanup: kill only services this script started
# --------------------------------------------------------------------------
cleanup() {
  local status=$?
  # Preserve failures from unguarded commands that trigger errexit.
  if [ "$status" -ne 0 ]; then
    EXIT_CODE=$status
  fi
  echo ""
  if [ "$STARTED_BACKEND" = true ] && [ -n "$BACKEND_PID" ]; then
    kill "$BACKEND_PID" 2>/dev/null && wait "$BACKEND_PID" 2>/dev/null || true
    echo "    Stopped backend (PID $BACKEND_PID)"
  fi
  if [ "$STARTED_FRONTEND" = true ] && [ -n "$FRONTEND_PID" ]; then
    kill "$FRONTEND_PID" 2>/dev/null && wait "$FRONTEND_PID" 2>/dev/null || true
    echo "    Stopped frontend (PID $FRONTEND_PID)"
  fi
  echo ""
  if [ "$EXIT_CODE" -eq 0 ]; then
    echo "✓ All checks passed."
  else
    echo "✗ Checks FAILED."
  fi
  exit "$EXIT_CODE"
}
trap cleanup EXIT

# --------------------------------------------------------------------------
# Utility: wait until a port responds
# --------------------------------------------------------------------------
wait_for_port() {
  local port=$1 name=$2 max_wait=${3:-60} path=${4:-/} pid=${5:-}
  local elapsed=0
  echo "    Waiting for $name on :$port..."
  while ! curl -sf "http://localhost:${port}${path}" > /dev/null 2>&1; do
    if [ -n "$pid" ] && ! ps -p "$pid" > /dev/null 2>&1; then
      echo "    ERROR: $name process exited before it was ready"
      STARTED_FRONTEND=false # a dead PID may be reused: never signal it
      EXIT_CODE=1
      exit 1
    fi
    sleep 1
    elapsed=$((elapsed + 1))
    if [ "$elapsed" -ge "$max_wait" ]; then
      echo "    ERROR: $name did not start within ${max_wait}s"
      EXIT_CODE=1
      exit 1
    fi
  done
  echo "    $name ready (${elapsed}s)"
}

# True when anything accepts connections on the port, whatever HTTP status it
# answers: curl exits 7 only when the connection is refused.
port_in_use() {
  local status=0
  curl -s -o /dev/null --max-time 3 "http://localhost:$1" || status=$?
  [ "$status" -ne 7 ]
}

# After the readiness probe: the launched frontend is still alive and a process
# in its tree owns the listener. A server that lost the bind race exits, and the
# probe would otherwise pass on the foreign listener that won it.
require_owned_frontend() {
  if ! ps -p "$FRONTEND_PID" > /dev/null 2>&1; then
    STARTED_FRONTEND=false
    echo "    ERROR: frontend process exited after the readiness probe; :$FRONTEND_PORT is not ours"
    EXIT_CODE=1
    exit 1
  fi
  local listeners listener cur
  # `ss` names the listener on Linux; `lsof` covers macOS. Liveness alone proves
  # nothing: a server that lost the bind race to a foreign listener is alive
  # until it exits. With no listener PID the run fails closed.
  listeners=$(ss -ltnpH "sport = :$FRONTEND_PORT" 2> /dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 || true)
  [ -n "$listeners" ] || listeners=$(lsof -nP -iTCP:"$FRONTEND_PORT" -sTCP:LISTEN -t 2> /dev/null || true)
  if [ -z "$listeners" ]; then
    echo "    ERROR: cannot establish which process listens on :$FRONTEND_PORT (neither ss nor lsof named one); refusing to trust the readiness probe"
    EXIT_CODE=1
    exit 1
  fi
  for listener in $listeners; do
    cur=$listener
    while [ -n "$cur" ] && [ "$cur" -gt 1 ]; do
      [ "$cur" = "$FRONTEND_PID" ] && return 0
      cur=$(ps -o ppid= -p "$cur" 2> /dev/null | tr -d ' ' || true)
    done
  done
  echo "    ERROR: the listener on :$FRONTEND_PORT is not owned by the frontend this script started"
  EXIT_CODE=1
  exit 1
}

# --------------------------------------------------------------------------
# Step 0: Ensure DB
# --------------------------------------------------------------------------
echo "==> Using env file: $ENV_FILE"
echo "==> Checking PostgreSQL..."
bash scripts/ensure-postgres.sh "$ENV_FILE"

# --------------------------------------------------------------------------
# Step 1: TypeScript typecheck
# --------------------------------------------------------------------------
echo ""
echo "==> [1/5] TypeScript typecheck..."
pnpm typecheck || { EXIT_CODE=1; exit 1; }

# --------------------------------------------------------------------------
# Step 2: TypeScript unit tests (Vitest)
# --------------------------------------------------------------------------
echo ""
echo "==> [2/5] TypeScript unit tests..."
pnpm test || { EXIT_CODE=1; exit 1; }

# --------------------------------------------------------------------------
# Step 3: Go tests
# --------------------------------------------------------------------------
echo ""
echo "==> [3/5] Go tests..."
echo "==> Verifying Go test wrapper..."
bash scripts/test-go.test.sh || { EXIT_CODE=1; exit 1; }
echo "==> Running database migrations..."
(cd server && go run ./cmd/migrate up) || { EXIT_CODE=1; exit 1; }
bash scripts/test-go.sh || { EXIT_CODE=1; exit 1; }

# --------------------------------------------------------------------------
# Step 4: Start services for E2E (backend reused if running; frontend always started here)
# --------------------------------------------------------------------------
echo ""
echo "==> [4/5] Starting services for E2E..."

if curl -sf "http://localhost:${PORT}/health" > /dev/null 2>&1; then
  echo "    Backend already running on :$PORT"
else
  echo "    Starting backend..."
  (cd server && go run ./cmd/server) > /tmp/multica-check-backend.log 2>&1 &
  BACKEND_PID=$!
  STARTED_BACKEND=true
  wait_for_port "$PORT" "Backend" 90 "/health"
fi

# E2E runs against a production build: `next dev` compiles each route on first
# visit, and the specs' 5 to 10s waits time out on that compile and on the slow
# dev client bundle. The script owns this server: a listener already on the port
# could be a dev server or anything else, so refuse it whatever status it
# answers and never kill or adopt it. `E2E_WEB_MODE=dev` is the dev smoke run.
E2E_WEB_MODE="${E2E_WEB_MODE:-prod}"
if port_in_use "$FRONTEND_PORT"; then
  echo "    ERROR: something already listens on :$FRONTEND_PORT; stop it so check.sh starts its own frontend"
  EXIT_CODE=1
  exit 1
fi
if [ "$E2E_WEB_MODE" = "dev" ]; then
  echo "    Starting frontend (next dev)..."
  pnpm dev:web > /tmp/multica-check-frontend.log 2>&1 &
  FRONTEND_PID=$!
  STARTED_FRONTEND=true
  wait_for_port "$FRONTEND_PORT" "Frontend" 120 "/" "$FRONTEND_PID"
  require_owned_frontend
else
  echo "    Building frontend (production)..."
  (cd apps/web && ./node_modules/.bin/next build --webpack) > /tmp/multica-check-frontend-build.log 2>&1 || {
    echo "    ERROR: production build failed; see /tmp/multica-check-frontend-build.log"
    EXIT_CODE=1
    exit 1
  }
  echo "    Starting frontend (next start)..."
  (cd apps/web && exec ./node_modules/.bin/next start --port "$FRONTEND_PORT") > /tmp/multica-check-frontend.log 2>&1 &
  FRONTEND_PID=$!
  STARTED_FRONTEND=true
  wait_for_port "$FRONTEND_PORT" "Frontend" 120 "/" "$FRONTEND_PID"
  require_owned_frontend
fi

# --------------------------------------------------------------------------
# Step 5: E2E tests (Playwright)
# --------------------------------------------------------------------------
echo ""
echo "==> [5/5] E2E tests (Playwright)..."
pnpm exec playwright test || { EXIT_CODE=1; exit 1; }
