#!/usr/bin/env bash
# Remove what this run created: the environment launch.sh registered, its
# processes and its database. Evidence stays. Usage: cleanup.sh
# Acts only on an environment that matches this run's ownership record, and
# stops only processes that carry this run's id (stop-pid.sh); never by name.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=ownership.sh
. "$(dirname "${BASH_SOURCE[0]}")/ownership.sh"
cd "$root"
marker="$(vm_marker)"
[ -f "$marker" ] || vm_refuse "no ownership record for this checkout; this run did not launch an environment here, so it will not remove one."
run_id="$(vm_value "$marker" RUN_ID)"; started="$(vm_value "$marker" STARTED_EPOCH)"
rec_name="$(vm_value "$marker" NAME)"; rec_created="$(vm_value "$marker" CREATED_AT)"; rec_db="$(vm_value "$marker" DB_NAME)"
[ -n "$run_id" ] || vm_refuse "the ownership record has no run id"

sweep() { # stop every process of this run; stop-pid.sh re-checks cwd and refuses a multica binary
  for pid in $(vm_owned_pids "$run_id"); do
    "$root/.agents/skills/verify-multica/scripts/stop-pid.sh" "$pid" "$root" || true
  done
}

m="$(vm_manifest_for_root)"
if [ -z "$m" ]; then   # launch never registered an environment: nothing to destroy
  sweep; rm -f "$marker"; echo "no environment registered; ownership record removed"; exit 0
fi
db="$(vm_value "$m" DB_NAME)"; api="$(vm_value "$m" BACKEND_PORT)"; web="$(vm_value "$m" FRONTEND_PORT)"
[ "$(vm_value "$m" OWNER)" = agent ] || vm_refuse "the registered environment is not agent-owned"
case "$db" in multica_*) ;; *) vm_refuse "database '$db' is not an isolated multica_* database" ;; esac
if [ -n "$rec_name" ]; then
  [ "$(vm_value "$m" NAME)" = "$rec_name" ] && [ "$(vm_value "$m" CREATED_AT)" = "$rec_created" ] && [ "$db" = "$rec_db" ] \
    || vm_refuse "the registered environment is not the one this run launched"
else  # launch stopped before recording it: accept only an environment created after this run began
  created="$(date -u -d "$(vm_value "$m" CREATED_AT)" +%s 2>/dev/null || echo 0)"
  [ "$created" -ge "$started" ] || vm_refuse "the registered environment predates this run"
fi

make down || true
sweep   # `down` can leave the Next.js server (a grandchild of turbo)
# X99 has no psql on PATH; destroy drops the database through one, so lend it the postgres container's.
shim="$(mktemp -d)"; trap 'rm -rf "$shim"' EXIT
printf '#!/usr/bin/env bash\nexec docker exec -i multica-postgres-1 psql "$@"\n' > "$shim/psql"; chmod +x "$shim/psql"
PATH="$shim:$PATH" make destroy ARGS="--yes"
rm -f "$marker"
for port in "$api" "$web"; do
  if ss -ltnH "sport = :$port" | grep -q .; then echo "CLEANUP FAIL: port $port still listening" >&2; exit 1; fi
  echo "port $port closed"
done
