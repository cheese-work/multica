#!/usr/bin/env bash
# Remove what this run created: the registered environment's processes and its
# database. Evidence stays. Usage: cleanup.sh
# Stops only pids whose cwd is inside this checkout (stop-pid.sh); never by name.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$root"
j="$(make status ARGS=--json)"
get() { printf '%s' "$j" | node -pe "JSON.parse(require('fs').readFileSync(0,'utf8')).$1"; }
api="$(get backend_port)"; web="$(get frontend_port)"; db="$(get database)"
make down || true
# `down` can leave the Next.js server (a grandchild of turbo): stop what still listens on our ports.
for port in "$api" "$web"; do
  for pid in $(ss -ltnpH "sport = :$port" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u); do
    "$root/.agents/skills/verify-multica/scripts/stop-pid.sh" "$pid" "$root"
  done
done
# Orphans of this checkout that no longer listen (turbo, next, playwright children).
for pid in $(pgrep -f "$root/(apps/web/)?node_modules/.*(turbo|next|playwright)" || true); do
  [ "$pid" = "$$" ] && continue
  "$root/.agents/skills/verify-multica/scripts/stop-pid.sh" "$pid" "$root" || true
done
# X99 has no psql on PATH; destroy drops the database through one, so lend it the postgres container's.
[ "$db" != multica ] || { echo "cleanup: refusing to drop the shared database" >&2; exit 1; }
shim="$(mktemp -d)"; trap 'rm -rf "$shim"' EXIT
printf '#!/usr/bin/env bash\nexec docker exec -i multica-postgres-1 psql "$@"\n' > "$shim/psql"; chmod +x "$shim/psql"
PATH="$shim:$PATH" make destroy ARGS="--yes"
for port in "$api" "$web"; do
  if ss -ltnH "sport = :$port" | grep -q .; then echo "CLEANUP FAIL: port $port still listening" >&2; exit 1; fi
  echo "port $port closed"
done
