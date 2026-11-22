#!/usr/bin/env bash
# Start this checkout's isolated stack (api + web) on X99 and warm the one route
# the drive needs. Run from anywhere. Prints the run dir, ports and database.
# Never run on C00. First run: pnpm install ~5 min, first page compile ~2 min.
set -euo pipefail
[ "$(hostname)" = "congvc-X99" ] || { echo "launch: X99 only (host is $(hostname))" >&2; exit 1; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$root"
# --ephemeral: owner=agent, 24 h TTL; the registry entry is the run dir.
make up ARGS="--ephemeral --ttl 2" || true   # a web start slower than the make timeout is retried below
for _ in $(seq 1 3); do
  if make status ARGS="--json" 2>/dev/null | grep -q '"web".*"running"'; then break; fi
  make up || true
done
eval "$(grep -E '^(NAME|BACKEND_PORT|FRONTEND_PORT|DB_NAME)=' "$HOME/.multica/dev/envs/$(make status ARGS=--json | node -pe 'JSON.parse(require("fs").readFileSync(0,"utf8")).name')/manifest.env")"
echo "run_dir=$HOME/.multica/dev/envs/$NAME api=http://localhost:$BACKEND_PORT web=http://localhost:$FRONTEND_PORT db=$DB_NAME"
# Warm the route the drive opens: `next dev` compiles on first visit (about 2 minutes).
curl -s -o /dev/null --max-time 400 "http://localhost:$FRONTEND_PORT/login" || echo "launch: warm-up of /login timed out" >&2
curl -s -o /dev/null --max-time 400 "http://localhost:$FRONTEND_PORT/e2e-warmup/chat" || true
