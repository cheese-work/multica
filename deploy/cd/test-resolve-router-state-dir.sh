#!/usr/bin/env bash
set -euo pipefail

# resolve-router-state-dir.sh (CHE-702): the router state dir must come from
# the running router's bind mount, not from the Compose checkout path, and a
# missing or ambiguous mount must refuse. docker is mocked; the state dirs
# are real directories.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir -p "$work_dir/bin"

# The mock answers `docker inspect <name> --format ...` with the lines in
# $RR_TEST_DIR/mounts (already filtered to /etc/nginx/router-state, as the
# real template does), or fails when $RR_TEST_DIR/missing exists.
cat >"$work_dir/bin/docker" <<'MOCK'
#!/usr/bin/env bash
[ "$1" = inspect ] || exit 99
[ -e "$RR_TEST_DIR/missing" ] && { echo "Error: No such object: $2" >&2; exit 1; }
cat "$RR_TEST_DIR/mounts"
MOCK
chmod +x "$work_dir/bin/docker"
export PATH="$work_dir/bin:$PATH" RR_TEST_DIR="$work_dir"

compose_state="$work_dir/compose checkout/deploy/cd/router/state"
live_state="$work_dir/ab checkout/deploy/cd/router/state"
mkdir -p "$compose_state" "$live_state"
printf '{"colour":"blue","backend_port":18091,"frontend_port":13001}\n' >"$live_state/active.json"

run() { bash deploy/cd/resolve-router-state-dir.sh 2>&1; }
refuses() {
  local name=$1 want=$2 output status
  set +e; output="$(run)"; status=$?; set -e
  [ "$status" -eq 1 ] || { echo "$name: exit $status, want 1: $output" >&2; exit 1; }
  [[ "$output" == *"$want"* ]] || { echo "$name: want '$want' in: $output" >&2; exit 1; }
}

# Differing paths: the live mount wins over the Compose checkout.
printf 'bind %s\n' "$live_state" >"$work_dir/mounts"
output="$(run)" || { echo "differing paths: want exit 0: $output" >&2; exit 1; }
[ "$output" = "$live_state" ] || { echo "differing paths: got '$output', want '$live_state'" >&2; exit 1; }

# Mount points at a dir without active.json (the Compose checkout, as in
# CD run 36083685740): refuse, never fall back.
printf 'bind %s\n' "$compose_state" >"$work_dir/mounts"
refuses no-active-json "has no readable active.json"

# No mount at the target.
: >"$work_dir/mounts"
refuses no-mount "has 0 mounts"

# Two mounts at the target.
printf 'bind %s\nbind %s\n' "$live_state" "$compose_state" >"$work_dir/mounts"
refuses ambiguous "has 2 mounts"

# A named volume is not a host path the controller can write.
printf 'volume %s\n' "$live_state" >"$work_dir/mounts"
refuses volume "not a host bind mount"

# Router container missing.
touch "$work_dir/missing"
refuses missing-container "not found"
rm "$work_dir/missing"

echo "resolve-router-state-dir fixtures passed"
