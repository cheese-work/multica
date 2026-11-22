#!/usr/bin/env bash
# Start this checkout's isolated stack (api + web) on X99 and warm the one route
# the drive needs. Run from anywhere. Prints the run dir, ports and database.
# Never run on C00. First run: pnpm install ~5 min, first page compile ~2 min.
# Refuses a checkout that already has an environment: that one is not this run's.
set -euo pipefail
if [ "${VERIFY_MULTICA_SKIP_HOST_CHECK:-}" != 1 ]; then  # the skip is for the unit tests only
  [ "$(hostname)" = "congvc-X99" ] || { echo "launch: X99 only (host is $(hostname))" >&2; exit 1; }
fi
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=ownership.sh
. "$(dirname "${BASH_SOURCE[0]}")/ownership.sh"
cd "$root"
[ -z "$(vm_manifest_for_root)" ] || vm_refuse "an environment is already registered for this checkout; it was not started by this run. Use a fresh checkout, or ask its owner."
marker="$(vm_marker)"
[ ! -e "$marker" ] || vm_refuse "an ownership record from an earlier run exists ($marker); run cleanup.sh first."
mkdir -p "$VM_STATE"
run_id="$(date +%s)-$$-$RANDOM"
{ printf 'RUN_ID=%q\n' "$run_id"; printf 'ROOT=%q\n' "$root"; printf 'STARTED_EPOCH=%q\n' "$(date -u +%s)"; } > "$marker"
export VERIFY_MULTICA_RUN_ID="$run_id"   # inherited by every process this run starts
# --ephemeral: owner=agent, 24 h TTL. Only reached on a checkout with no environment.
make up ARGS="--ephemeral --ttl 2" || true   # a web start slower than the make timeout is retried below
for _ in $(seq 1 3); do
  if make status ARGS="--json" 2>/dev/null | grep -q '"web".*"running"'; then break; fi
  make up || true
done
m="$(vm_manifest_for_root)"
[ -n "$m" ] || { echo "launch: make up registered no environment; see the ownership record $marker, then run cleanup.sh" >&2; exit 1; }
[ "$(vm_value "$m" OWNER)" = agent ] || vm_refuse "the registered environment is not agent-owned"
NAME="$(vm_value "$m" NAME)"; CREATED_AT="$(vm_value "$m" CREATED_AT)"; DB_NAME="$(vm_value "$m" DB_NAME)"
BACKEND_PORT="$(vm_value "$m" BACKEND_PORT)"; FRONTEND_PORT="$(vm_value "$m" FRONTEND_PORT)"
{ printf 'NAME=%q\n' "$NAME"; printf 'CREATED_AT=%q\n' "$CREATED_AT"; printf 'DB_NAME=%q\n' "$DB_NAME"; } >> "$marker"
echo "run_dir=$VM_ENVS/$NAME api=http://localhost:$BACKEND_PORT web=http://localhost:$FRONTEND_PORT db=$DB_NAME"
# Warm the route the drive opens: `next dev` compiles on first visit (about 2 minutes).
curl -s -o /dev/null --max-time 400 "http://localhost:$FRONTEND_PORT/login" || echo "launch: warm-up of /login timed out" >&2
curl -s -o /dev/null --max-time 400 "http://localhost:$FRONTEND_PORT/e2e-warmup/chat" || true
