#!/usr/bin/env bash
# Registry-level behaviour of scripts/dev-env.sh, with no services started.
#
# Everything here runs against a throwaway MULTICA_DEV_HOME holding hand-written
# manifests, so the verbs are exercised end to end without a database, a
# backend, or a port.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

export MULTICA_DEV_HOME="$tmp_dir/dev"
export MULTICA_DEV_WORKSPACES_PARENT="$tmp_dir/workspaces-parent"
export MULTICA_DEV_DESKTOP_APP_DATA="$tmp_dir/app-data"
export MULTICA_DEV_PROFILES_HOME="$tmp_dir/profiles"

fake_bin="$tmp_dir/bin"
mkdir -p "$fake_bin"
cat > "$fake_bin/psql" <<'EOF'
#!/usr/bin/env bash
case " $* " in
  *" DROP DATABASE "*) [ "${FAIL_DROP:-0}" != 1 ] ;;
  *) printf '1\n' ;;
esac
EOF
chmod +x "$fake_bin/psql"
export PATH="$fake_bin:$PATH"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_contains() {
  local file=$1 expected=$2
  if ! grep -Fq "$expected" "$file"; then
    echo "Expected output to contain: $expected" >&2
    echo "Observed:" >&2
    sed 's/^/  /' "$file" >&2
    exit 1
  fi
}

dev_env() {
  bash "$root_dir/scripts/dev-env.sh" "$@"
}

write_manifest() {
  local name=$1 dir=$2 offset=$3
  local profile="dev-dev-env-test-$offset"
  mkdir -p "$MULTICA_DEV_HOME/envs/$name/logs"
  cat > "$MULTICA_DEV_HOME/envs/$name/manifest.env" <<EOF
NAME=$name
DIR=$(printf '%q' "$dir")
CREATED_AT=2026-01-01T00:00:00Z
OWNER=agent
TTL_HOURS=0
ENV_FILE=.env.example
OFFSET=$offset
BACKEND_PORT=$((18080 + offset))
FRONTEND_PORT=$((13000 + offset))
DB_NAME=multica_dev_env_test_$offset
DATABASE_URL=postgres://multica:multica@localhost:5432/multica_dev_env_test_$offset?sslmode=disable
PROFILE=$profile
WORKSPACES_ROOT=$(printf '%q' "$MULTICA_DEV_WORKSPACES_PARENT/multica_workspaces_$profile")
DESKTOP_RENDERER_PORT=$((5174 + offset))
DESKTOP_APP_SUFFIX=$name
EOF
}

out="$tmp_dir/out"

assert_listener_ownership() {
  local case_name=$1 expected=$2 launcher=$3 listener=$4 listener_pgid=$5 recorded=${6:-}
  (
    # shellcheck disable=SC1090
    source "$root_dir/scripts/dev-env.sh"
    STATE_DIR="$tmp_dir/ownership-$case_name"
    mkdir -p "$STATE_DIR"
    [ -z "$recorded" ] || printf '%s\n' "$recorded" > "$(listener_pid_file web)"
    TEST_LAUNCHER=$launcher
    TEST_LISTENER=$listener
    TEST_LISTENER_PGID=$listener_pgid
    TEST_CASE=$case_name

    component_pid() { printf '%s' "$TEST_LAUNCHER"; }
    port_listener_pid() { printf '%s' "$TEST_LISTENER"; }
    process_group_id() { printf '%s' "$TEST_LISTENER_PGID"; }
    process_parent_id() {
      case "$TEST_CASE:$1" in
        nested:420) printf '310' ;;
        nested:310) printf '200' ;;
        nested:200) printf '%s' "$TEST_LAUNCHER" ;;
        *) printf '1' ;;
      esac
    }

    local actual=external
    if listener_belongs_to_component web 13000; then actual=owned; fi
    [ "$actual" = "$expected" ] \
      || fail "$case_name listener ownership = $actual, want $expected"
  )
}

assert_port_lookup() {
  local case_name=$1 want_pid=$2 want_free=$3 lsof_out=$4 ss_out=$5
  (
    # shellcheck disable=SC1090
    source "$root_dir/scripts/dev-env.sh"
    TEST_LSOF=$lsof_out
    TEST_SS=$ss_out
    # Stub both lookups. The stubs ignore their filters on purpose, so the
    # lookup itself must enforce the exact port and the LISTEN state.
    lsof() { [ -z "$TEST_LSOF" ] || printf '%s\n' "$TEST_LSOF"; }
    ss() { [ -z "$TEST_SS" ] || printf '%s\n' "$TEST_SS"; }

    local pid free=busy
    pid="$(port_listener_pid 13000)"
    [ "$pid" = "$want_pid" ] \
      || fail "$case_name port_listener_pid = '$pid', want '$want_pid'"
    if port_free 13000; then free=free; fi
    [ "$free" = "$want_free" ] \
      || fail "$case_name port_free = $free, want $want_free"
  )
}

assert_nested_listener_is_recorded() (
  # shellcheck disable=SC1090
  source "$root_dir/scripts/dev-env.sh"
  STATE_DIR="$tmp_dir/ownership-record"
  mkdir -p "$STATE_DIR"

  component_pid() { printf '100'; }
  port_listener_pid() { printf '420'; }
  process_group_id() { printf '310'; }
  process_parent_id() {
    case "$1" in
      420) printf '310' ;;
      310) printf '200' ;;
      200) printf '100' ;;
      *) printf '1' ;;
    esac
  }

  local claimed
  claimed="$(record_component_listener web 13000)" \
    || fail "nested listener was not claimed"
  [ "$claimed" = 420 ] || fail "claimed listener = $claimed, want 420"
  [ "$(cat "$(listener_pid_file web)")" = 420 ] \
    || fail "nested listener pid was not recorded"
)

assert_stop_handles_listener() {
  local case_name=$1 listener=$2 listener_pgid=$3 listener_parent=$4 recorded=${5:-}
  local expected_target=${6:-}
  (
    # shellcheck disable=SC1090
    source "$root_dir/scripts/dev-env.sh"
    STATE_DIR="$tmp_dir/stop-$case_name"
    mkdir -p "$STATE_DIR"
    BACKEND_PORT=18080
    FRONTEND_PORT=13000
    DESKTOP_RENDERER_PORT=5174
    local signals="$STATE_DIR/signals"
    [ -z "$recorded" ] || printf '%s\n' "$recorded" > "$(listener_pid_file web)"
    TEST_CASE=$case_name
    TEST_LISTENER=$listener
    TEST_LISTENER_PGID=$listener_pgid
    TEST_LISTENER_PARENT=$listener_parent

    component_pid() { [ "$TEST_CASE" = nested ] && printf '100'; }
    port_listener_pid() { printf '%s' "$TEST_LISTENER"; }
    process_group_id() { printf '%s' "$TEST_LISTENER_PGID"; }
    process_parent_id() {
      case "$1" in
        "$TEST_LISTENER") printf '%s' "$TEST_LISTENER_PARENT" ;;
        "$TEST_LISTENER_PARENT") printf '100' ;;
        *) printf '1' ;;
      esac
    }
    sleep() { :; }
    kill() {
      [ "$1" != -0 ] || return 1
      printf 'signal=%s target=%s\n' "$1" "${2:-}" >> "$signals"
    }

    stop_component web > "$out" 2>&1 || fail "$case_name stop failed"
    if [ -n "$expected_target" ]; then
      require_contains "$signals" "target=$expected_target"
    elif [ -s "$signals" ]; then
      fail "$case_name stop signalled an external listener: $(cat "$signals")"
    fi
  )
}

# ---------------------------------------------------------------------------
# An empty registry is a normal state, not an error.
# ---------------------------------------------------------------------------
dev_env list > "$out" 2>&1 || fail "list on an empty registry must succeed"
require_contains "$out" "No environments registered"

dev_env list --json > "$out" 2>&1 || fail "list --json on an empty registry must succeed"
if [ "$(cat "$out")" != "[]" ]; then
  fail "list --json on an empty registry = $(cat "$out"), want []"
fi

# ---------------------------------------------------------------------------
# Manifest serialization and user-provided names are safe. A manifest is
# sourced by Bash, so values must be shell-escaped and a name must never be
# able to walk outside envs/ before destroy eventually runs rm -rf.
# ---------------------------------------------------------------------------
quoted="$tmp_dir/quoted.env"
dangerous='a path with spaces;$(touch should-not-exist)'
bash -c 'source "$1"; write_manifest_value DIR "$2"' _ "$root_dir/scripts/dev-env.sh" "$dangerous" > "$quoted"
loaded="$(bash -c 'source "$1"; printf %s "$DIR"' _ "$quoted")"
[ "$loaded" = "$dangerous" ] || fail "manifest value did not round-trip safely"
[ ! -e "$root_dir/should-not-exist" ] || fail "loading a manifest executed its value"

status=0
dev_env up --name ../../escape > "$out" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail "up accepted a path-traversing environment name"
require_contains "$out" "Invalid environment name"

status=0
dev_env up --ttl nope > "$out" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail "up accepted a non-numeric TTL"
require_contains "$out" "TTL must be a positive integer"

# Rewriting an allocated database name must preserve the existing connection
# endpoint, credentials and query parameters.
rewritten="$(bash -c 'source "$1"; database_url_with_name "$2" "$3"' _ \
  "$root_dir/scripts/dev-env.sh" \
  'postgres://dev:p%40ss@127.0.0.1:55432/old_db?sslmode=require&application_name=dev' \
  'new_db')"
[ "$rewritten" = 'postgres://dev:p%40ss@127.0.0.1:55432/new_db?sslmode=require&application_name=dev' ] \
  || fail "database URL rewrite changed more than the database name: $rewritten"

# ---------------------------------------------------------------------------
# A registered environment is visible to both renderings, and the JSON one
# parses — agents read it, so a stray log line in it is a broken contract.
# ---------------------------------------------------------------------------
write_manifest "probe-901" "$tmp_dir/checkout" 901
mkdir -p "$tmp_dir/checkout"

dev_env list > "$out" 2>&1 || fail "list must succeed with one environment"
require_contains "$out" "probe-901"
require_contains "$out" "18981"

dev_env status probe-901 --json > "$out" 2>&1 || fail "status --json must succeed"
node -e '
  const fs = require("fs");
  const payload = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  if (payload.name !== "probe-901") throw new Error("name = " + payload.name);
  if (payload.backend_port !== 18981) throw new Error("backend_port = " + payload.backend_port);
  for (const key of ["api", "web", "daemon", "desktop"]) {
    if (!payload.components[key]) throw new Error("missing component " + key);
    if (payload.components[key].state !== "stopped") {
      throw new Error(key + " state = " + payload.components[key].state);
    }
  }
' "$out" || fail "status --json is not machine-readable"

# ---------------------------------------------------------------------------
# Stopping an environment that is not running is a no-op that SUCCEEDS.
#
# This is the regression that made `make down` exit 1 after reporting success:
# on bash 3.2 a command substitution whose function ends in a failing command
# aborts the whole script under `set -e`, and "no process is listening on this
# port" is that function's normal answer.
# ---------------------------------------------------------------------------
status=0
dev_env down probe-901 --components api,web > "$out" 2>&1 || status=$?
if [ "$status" -ne 0 ]; then
  echo "Observed:" >&2
  sed 's/^/  /' "$out" >&2
  fail "down on a stopped environment exited $status, want 0"
fi
require_contains "$out" "stopped"

# Commands launched through env-exec must not inherit the daemon-task identity
# hints that make human/profile CLI commands reject --profile.
write_manifest "clean-env-903" "$root_dir" 903
MULTICA_TASK_CONFIG_ROOT=/task/config \
MULTICA_TASK_WORKSPACES_ROOT=/task/workspaces \
MULTICA_WORKSPACES_ROOT=/owner/workspaces \
  dev_env exec clean-env-903 -- sh -c '
    test -z "${MULTICA_TASK_CONFIG_ROOT:-}" &&
    test -z "${MULTICA_TASK_WORKSPACES_ROOT:-}" &&
    test "$MULTICA_WORKSPACES_ROOT" = "$1"
  ' _ "$MULTICA_DEV_WORKSPACES_PARENT/multica_workspaces_dev-dev-env-test-903" \
  > "$out" 2>&1 || fail "env-exec leaked daemon task identity or owner workspaces root"

# A health response without process identity is never proof that the process is
# this checkout's freshly launched API.
if bash -c 'source "$1"; api_started_after '\''{"status":"ok"}'\'' 1' _ "$root_dir/scripts/dev-env.sh"; then
  fail "legacy /health without started_at was accepted as current"
fi

# Listener ownership follows the process tree, not only the launcher's process
# group. Turbo/pnpm can create a nested process group for Next while keeping the
# listener below the launcher in the PPID chain.
assert_listener_ownership same-pgid owned 100 200 100
assert_listener_ownership nested owned 100 420 310
assert_listener_ownership recorded owned 100 200 200 200
assert_listener_ownership external external 100 999 999
assert_nested_listener_is_recorded

# Listener lookup must not depend on lsof alone. X99's lsof 4.95.0 prints no pid
# for a listener whose process name contains " (", which is how Next names its
# server ("next-server (v16.3.8)"), so ss is consulted as well. The lookup
# keeps exact port matching across IPv4 and IPv6, and fails closed: two
# distinct listeners or a listener with no visible owner never yield a pid,
# and such a port is never reported free.
assert_port_lookup lsof-only 4100 busy '4100' ''
assert_port_lookup parenthesized-name 4242 busy '' \
  'LISTEN 0 511 127.0.0.1:13000 0.0.0.0:* users:(("next-server (v1",pid=4242,fd=19))'
assert_port_lookup ipv6-wildcard 4243 busy '' \
  'LISTEN 0 511 [::]:13000 [::]:* users:(("next-server (v1",pid=4243,fd=19))'
assert_port_lookup ipv4-mapped 4244 busy '' \
  'LISTEN 0 511 [::ffff:127.0.0.1]:13000 *:* users:(("node",pid=4244,fd=20))'
assert_port_lookup both-tools-agree 4245 busy '4245' \
  'LISTEN 0 511 *:13000 *:* users:(("node",pid=4245,fd=20))'
assert_port_lookup other-port '' free '' \
  'LISTEN 0 511 127.0.0.1:130001 0.0.0.0:* users:(("next-server (v1",pid=4246,fd=19))
LISTEN 0 511 127.0.0.1:113000 0.0.0.0:* users:(("node",pid=4247,fd=19))'
assert_port_lookup two-listeners '' busy '4248
4249' ''
assert_port_lookup two-listeners-across-tools '' busy '4250' \
  'LISTEN 0 511 [::]:13000 [::]:* users:(("node",pid=4251,fd=20))'
assert_port_lookup unknown-owner '' busy '' \
  'LISTEN 0 511 0.0.0.0:13000 0.0.0.0:*'
assert_port_lookup nothing '' free '' ''
# One visible pid plus a listener whose owner is hidden is not a proved owner.
assert_port_lookup visible-and-hidden-lsof '' busy '4252' \
  'LISTEN 0 511 0.0.0.0:13000 0.0.0.0:*'
assert_port_lookup visible-and-hidden-ss '' busy '' \
  'LISTEN 0 511 [::]:13000 [::]:* users:(("node",pid=4253,fd=20))
LISTEN 0 511 0.0.0.0:13000 0.0.0.0:*'
assert_port_lookup one-owner-two-families 4254 busy '' \
  'LISTEN 0 511 [::]:13000 [::]:* users:(("node",pid=4254,fd=20))
LISTEN 0 511 0.0.0.0:13000 0.0.0.0:* users:(("node",pid=4254,fd=21))'

# Only LISTEN rows count. An established connection on the port, even one owned
# by another process, is neither a listener nor a reason to call the port busy.
assert_port_lookup established-only '' free '' \
  'ESTAB 0 0 127.0.0.1:13000 127.0.0.1:52344 users:(("node",pid=4255,fd=30))'
assert_port_lookup listen-and-established 4256 busy '' \
  'LISTEN 0 511 *:13000 *:* users:(("node",pid=4256,fd=20))
ESTAB 0 0 127.0.0.1:13000 127.0.0.1:52345 users:(("curl",pid=4257,fd=3))'

# Stopping first records an owned nested listener before killing the launcher's
# process group. An unrelated port occupant never receives a signal.
assert_stop_handles_listener nested 420 310 200 "" 420
assert_stop_handles_listener external 999 999 1 888

# ---------------------------------------------------------------------------
# Unknown names and components fail loudly instead of doing something else.
# ---------------------------------------------------------------------------
status=0
dev_env status no-such-env > "$out" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail "status on an unknown environment must fail"
require_contains "$out" "Unknown environment"

status=0
dev_env up --components nope > "$out" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail "up with an unknown component must fail"
require_contains "$out" "Unknown component"

# ---------------------------------------------------------------------------
# gc reports what it would collect and touches nothing in --dry-run. An
# environment whose checkout is gone has no owner left to stop it, which is how
# 152 databases accumulated with nothing on the machine able to list them.
# ---------------------------------------------------------------------------
write_manifest "orphan-902" "$tmp_dir/deleted-checkout" 902

dev_env gc --dry-run > "$out" 2>&1 || fail "gc --dry-run must succeed"
require_contains "$out" "orphan-902 would be collected"
if grep -Fq "probe-901 would be collected" "$out"; then
  fail "gc must not collect an environment whose directory still exists"
fi
[ -f "$MULTICA_DEV_HOME/envs/orphan-902/manifest.env" ] || fail "gc --dry-run deleted a manifest"

# A failed database drop keeps the manifest and slot so cleanup can be retried;
# destroy must never print success and forget the only deletion recipe.
write_manifest "drop-fails-904" "$root_dir" 904
status=0
FAIL_DROP=1 dev_env destroy drop-fails-904 --yes > "$out" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail "destroy succeeded after DROP DATABASE failed"
[ -f "$MULTICA_DEV_HOME/envs/drop-fails-904/manifest.env" ] \
  || fail "destroy discarded the manifest after DROP DATABASE failed"
require_contains "$out" "manifest and slot were kept"
dev_env destroy drop-fails-904 --yes > "$out" 2>&1 || fail "retrying destroy after database recovery failed"

# ---------------------------------------------------------------------------
# destroy consumes the manifest: the slot is free afterwards, which is what
# makes the registry an allocator rather than a second place to leak.
# ---------------------------------------------------------------------------
dev_env destroy probe-901 --yes > "$out" 2>&1 || fail "destroy must succeed"
[ ! -d "$MULTICA_DEV_HOME/envs/probe-901" ] || fail "destroy left the environment directory behind"

dev_env list > "$out" 2>&1 || fail "list must succeed after destroy"
if grep -Fq "probe-901" "$out"; then
  fail "destroyed environment is still listed"
fi

# Declining the confirmation is a successful no-op, not a failure.
printf 'n\n' | dev_env destroy orphan-902 > "$out" 2>&1 || fail "declining destroy must exit 0"
require_contains "$out" "Cancelled."
[ -d "$MULTICA_DEV_HOME/envs/orphan-902" ] || fail "declined destroy removed the environment anyway"

# ---------------------------------------------------------------------------
# Startup must never run destructive global GC on its own. `up` only starts
# THIS checkout's environment; collecting other registered environments (their
# profile, daemon workspaces, Desktop userData and database) is an explicit
# `gc` or `up --gc`, because the collector cannot tell a stale environment from
# one whose checkout is merely on an unmounted disk or a path a runner reuses.
#
# Everything below runs in a copy of the script inside a throwaway "repo", with
# synthetic environments and stub commands, so a regression deletes only
# fixtures under $tmp_dir.
# ---------------------------------------------------------------------------
fake_repo="$tmp_dir/fake-repo"
stub_go_dir="$tmp_dir/stub-go"
mkdir -p "$fake_repo/scripts" "$fake_repo/server" "$stub_go_dir"
cp "$root_dir/scripts/dev-env.sh" "$fake_repo/scripts/dev-env.sh"
cp "$root_dir/scripts/local-env.sh" "$fake_repo/scripts/local-env.sh"
cp "$root_dir/.env.example" "$fake_repo/.env.example"
# `go` fails fast so `up` stops at the migration step, after GC has had its
# chance to run and before any service would start.
cat > "$stub_go_dir/go" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$stub_go_dir/go"

make_stale_env() {
  local name=$1 offset=$2 profile
  write_manifest "$name" "$tmp_dir/gone-$name" "$offset"
  profile="dev-dev-env-test-$offset"
  mkdir -p "$MULTICA_DEV_PROFILES_HOME/$profile" \
    "$MULTICA_DEV_WORKSPACES_PARENT/multica_workspaces_$profile" \
    "$MULTICA_DEV_DESKTOP_APP_DATA/Multica Canary $name"
}

stale_env_intact() {
  local name=$1 offset=$2 profile="dev-dev-env-test-$2"
  [ -f "$MULTICA_DEV_HOME/envs/$name/manifest.env" ] \
    && [ -d "$MULTICA_DEV_PROFILES_HOME/$profile" ] \
    && [ -d "$MULTICA_DEV_WORKSPACES_PARENT/multica_workspaces_$profile" ] \
    && [ -d "$MULTICA_DEV_DESKTOP_APP_DATA/Multica Canary $name" ]
}

# HOME and the dev tmp dir point into $tmp_dir too, so no default path in the
# script can resolve to the real home directory.
fake_up() {
  (cd "$fake_repo" && HOME="$tmp_dir/home" MULTICA_DEV_TMPDIR="$tmp_dir/dev-tmp" \
    PATH="$stub_go_dir:$PATH" bash scripts/dev-env.sh up --components api "$@")
}

make_stale_env "stale-910" 910

status=0
fake_up > "$out" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail "stubbed up unexpectedly succeeded; the migration stub should stop it"
require_contains "$out" "Migrations failed"
stale_env_intact stale-910 910 \
  || fail "default up destroyed another environment's manifest, profile, workspaces or userData"
if grep -Fq "Destroying" "$out"; then
  fail "default up ran destroy"
fi
# The stale environment is still worth telling the user about.
require_contains "$out" "stale-910 would be collected"
require_contains "$out" "up --gc"

# gc --dry-run reports and touches nothing, including profile and userData.
dev_env gc --dry-run > "$out" 2>&1 || fail "gc --dry-run must succeed"
require_contains "$out" "stale-910 would be collected"
stale_env_intact stale-910 910 || fail "gc --dry-run deleted environment state"

# The explicit opt-in still collects, and only the stale environment.
mkdir -p "$tmp_dir/live-checkout"
write_manifest "live-911" "$tmp_dir/live-checkout" 911
status=0
fake_up --gc > "$out" 2>&1 || status=$?
require_contains "$out" "stale-910: its checkout directory is gone"
[ ! -e "$MULTICA_DEV_HOME/envs/stale-910" ] || fail "up --gc left the stale manifest"
[ ! -e "$MULTICA_DEV_PROFILES_HOME/dev-dev-env-test-910" ] || fail "up --gc left the stale profile"
[ ! -e "$MULTICA_DEV_WORKSPACES_PARENT/multica_workspaces_dev-dev-env-test-910" ] || fail "up --gc left the stale workspaces"
[ ! -e "$MULTICA_DEV_DESKTOP_APP_DATA/Multica Canary stale-910" ] || fail "up --gc left the stale userData"
[ -f "$MULTICA_DEV_HOME/envs/live-911/manifest.env" ] || fail "up --gc collected an environment whose checkout exists"

# Manual gc is unchanged.
make_stale_env "stale-912" 912
dev_env gc > "$out" 2>&1 || fail "manual gc must succeed"
require_contains "$out" "stale-912: its checkout directory is gone"
[ ! -e "$MULTICA_DEV_HOME/envs/stale-912" ] || fail "manual gc left the stale manifest"

echo "✓ dev-env.sh registry behaviour verified"
