#!/usr/bin/env bash
# Fixture checks for record-checks.sh. The failure this guards against is run
# 35822016641: the checks were snapshotted while CI's `backend` was still in
# flight, and admission refused a commit whose backend later passed. A stub
# `gh` serves one check-runs response per poll so each case replays a
# timeline; the recorder must wait through pending checks and stop on
# terminal ones, and admission must still refuse a terminal failure.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

bash deploy/cd/ensure-pyyaml.sh

source_sha="504078f8ea7fa31f342f195659e93a7f6c3e5a91"
stub_dir="$tmp_dir/bin"
mkdir -p "$stub_dir"
cat >"$stub_dir/gh" <<'EOF'
#!/usr/bin/env bash
# `gh api <path> ...`: the commit endpoint returns a server-only file list;
# check-runs returns the next queued poll response, repeating the last.
case "$2" in
  */check-runs*)
    count=$(( $(cat "$STUB_DIR/polls") + 1 ))
    echo "$count" >"$STUB_DIR/polls"
    total=$(ls "$STUB_DIR"/poll-* | wc -l)
    cat "$STUB_DIR/poll-$(( count < total ? count : total ))"
    ;;
  *) echo '{"files":["server/only.go"],"truncated":false}' ;;
esac
EOF
chmod +x "$stub_dir/gh"

# $1 = case name; remaining args = one poll response each, as `name=conclusion`
# pairs (conclusion `null` means still running; a missing name means no run).
replay() {
  local name=$1 i=0
  shift
  rm -f "$stub_dir"/poll-*
  echo 0 >"$stub_dir/polls"
  for poll in "$@"; do
    i=$(( i + 1 ))
    : >"$stub_dir/poll-$i"
    for pair in $poll; do
      local conclusion="${pair#*=}"
      [ "$conclusion" = null ] || conclusion="\"$conclusion\""
      echo "{\"name\":\"${pair%%=*}\",\"conclusion\":$conclusion}" >>"$stub_dir/poll-$i"
    done
  done
  PATH="$stub_dir:$PATH" STUB_DIR="$stub_dir" GITHUB_REPOSITORY=cheese-work/multica \
    CHECKS_POLL_SECONDS=0 CHECKS_WAIT_SECONDS="${WAIT:-60}" \
    bash deploy/cd/record-checks.sh "$source_sha" "$tmp_dir/$name.json" >"$tmp_dir/$name.log"
}

conclusion_of() {
  node -e 'const c=JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8"));process.stdout.write(String(c.contexts[process.argv[2]]))' \
    "$tmp_dir/$1.json" "$2"
}

expect() {
  local name=$1 check=$2 want=$3 polls=$4 got
  got="$(conclusion_of "$name" "$check")"
  [ "$got" = "$want" ] || { echo "$name: $check want $want, got $got" >&2; exit 1; }
  got="$(cat "$stub_dir/polls")"
  [ "$got" = "$polls" ] || { echo "$name: want $polls polls, got $got" >&2; exit 1; }
}

# The 5c834a2f timeline: cd-qualification done, backend not created yet, then
# running, then success. The snapshot must be the settled one.
replay backend-late \
  "cd-qualification=success frontend=null" \
  "cd-qualification=success frontend=success backend=null" \
  "cd-qualification=success frontend=success backend=success"
expect backend-late backend success 3
grep -q "waiting for required checks: backend frontend" "$tmp_dir/backend-late.log"

# A terminal failure ends the wait at once — nothing more to wait for — and
# the snapshot records it, so admission refuses before any cutover.
replay backend-failed \
  "cd-qualification=success frontend=success backend=failure" \
  "cd-qualification=success frontend=success backend=success"
expect backend-failed backend failure 1

# `mobile` is path-excluded for a server-only commit, so its absence does not
# hold the wait open.
replay mobile-excluded "cd-qualification=success frontend=success backend=success"
expect mobile-excluded mobile undefined 1

# A check still running at the deadline is recorded as running, not dropped.
WAIT=0 replay deadline "cd-qualification=success frontend=success backend=null"
expect deadline backend null 1
grep -q "still pending after 0s: backend" "$tmp_dir/deadline.log"

echo "record-checks fixtures passed"
