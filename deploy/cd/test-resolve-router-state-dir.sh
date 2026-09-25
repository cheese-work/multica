#!/usr/bin/env bash
set -euo pipefail

# resolve-router-state-dir.sh (CHE-773 follow-up): the router's state
# directory must come from the running container's own bind mount, never a
# path derived from $C00_COMPOSE_DIR — router/docker-compose.router.yml is a
# separate, by-hand C00 adoption step that can live anywhere. CD run
# 36083685740 attempt 2 assumed C00_COMPOSE_DIR/deploy/cd/router/state and
# refused before drain against a path the router was never mounted at, while
# the router already had a healthy, adopted active.json at its real
# (different) mount. `docker` here is mocked to model exactly that mismatch
# without a real Docker daemon or a real long-running router container.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir -p "$work_dir/bin"

real_mount="$work_dir/real-mount"
mkdir -p "$real_mount"

cat >"$work_dir/bin/docker" <<MOCK
#!/usr/bin/env bash
if [ "\$1" = "inspect" ]; then
  case "\${*}" in
    *"multica-ab-router"*) printf '%s\n' "$real_mount" ;;
    *"missing-container"*) exit 1 ;;
    *"dangling-container"*) printf '%s\n' "$work_dir/does-not-exist" ;;
    *) exit 1 ;;
  esac
  exit 0
fi
exit 1
MOCK
chmod +x "$work_dir/bin/docker"
export PATH="$work_dir/bin:$PATH"

# Happy path: container is running and its bind-mount source exists.
output="$(bash deploy/cd/resolve-router-state-dir.sh)"
[ "$output" = "$real_mount" ] || { echo "happy path: got '$output', want '$real_mount'" >&2; exit 1; }

# Container not found (stopped, wrong name) — must fail loudly, never guess
# a derived path.
set +e
output="$(bash deploy/cd/resolve-router-state-dir.sh --container missing-container 2>&1)"
status=$?
set -e
[ "$status" -eq 1 ] || { echo "missing container: exit $status, want 1" >&2; exit 1; }
[[ "$output" == *"could not resolve"* ]] || { echo "missing container: $output" >&2; exit 1; }

# Container found, but its reported mount source does not exist on this
# host (a stale inspect result, or a container inspected from the wrong
# host) — must also fail loudly rather than hand back an unusable path.
set +e
output="$(bash deploy/cd/resolve-router-state-dir.sh --container dangling-container 2>&1)"
status=$?
set -e
[ "$status" -eq 1 ] || { echo "dangling mount: exit $status, want 1" >&2; exit 1; }
[[ "$output" == *"does not exist on this host"* ]] || { echo "dangling mount: $output" >&2; exit 1; }

echo "resolve-router-state-dir.sh fixtures passed"
