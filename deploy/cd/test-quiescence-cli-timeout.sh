#!/usr/bin/env bash
set -euo pipefail

# Regression test for CHE-823 (run 36437999695): cutover.sh calls
# `quiescence.mjs preflight|final-gate --psql-via-docker-exec` WITHOUT
# --query-timeout-ms, so the CLI default governs the outer wall-clock
# `timeout` around `docker exec ... psql`. That default was the 500ms SQL
# statement budget instead of DEFAULT_WALL_CLOCK_TIMEOUT_MS, so ordinary
# docker exec latency on a busy C00 was reported as "observation query
# exceeded wall-clock timeout" and the preflight denied a healthy cutover.
#
# Stub-driven: a fake `docker` on PATH sleeps past 500ms (but well under
# 2000ms) and returns an empty observation, so no Docker daemon or
# Postgres is needed. The server-side statement_timeout must stay 500ms.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

stub_dir="$(mktemp -d)"
trap 'rm -rf "$stub_dir"' EXIT

cat >"$stub_dir/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$(dirname "$0")/docker-args"
sleep 0.8
STUB
chmod +x "$stub_dir/docker"

for command in preflight final-gate; do
  set +e
  output="$(PATH="$stub_dir:$PATH" node deploy/cd/quiescence.mjs "$command" \
    --database-url postgres://synthetic@localhost:5432/synthetic \
    --psql-via-docker-exec synthetic-postgres 2>&1)"
  status=$?
  set -e

  if grep -q "exceeded wall-clock timeout" <<<"$output"; then
    echo "FAIL: $command with the CLI default timeout killed a 0.8s docker exec: $output" >&2
    exit 1
  fi
  if [ "$status" -ne 0 ]; then
    echo "FAIL: $command expected admit on an empty observation (exit $status): $output" >&2
    exit 1
  fi
  if ! grep -qx -- "PGOPTIONS=-c statement_timeout=500 -c lock_timeout=500 -c idle_in_transaction_session_timeout=500" "$stub_dir/docker-args"; then
    echo "FAIL: $command server-side statement budget is no longer 500ms:" >&2
    cat "$stub_dir/docker-args" >&2
    exit 1
  fi
  echo "PASS: $command default wall-clock covers docker exec overhead; statement_timeout stays 500ms"
done
