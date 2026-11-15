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
  # Inject a rate limit on either request, including a partially written page.
  *)
    endpoint=commit
    [[ "$2" != */check-runs* ]] || endpoint=checks
    if [ "${RATE_ENDPOINT:-}" = "$endpoint" ] && [ ! -f "$STUB_DIR/rate-seen" ]; then
      touch "$STUB_DIR/rate-seen"
      if [ "$endpoint" = checks ]; then
        echo 'HTTP/2.0 200 OK'
        echo
        echo '{"name":"backend","conclusion":"failure","id":999}'
      fi
      echo "HTTP/2.0 ${RATE_STATUS:-403} Forbidden"
      echo "X-RateLimit-Remaining: ${RATE_REMAINING:-0}"
      echo "X-RateLimit-Reset: $(( $(date +%s) + ${RATE_RESET_OFFSET:--1} ))"
      [ -z "${RATE_RETRY_AFTER:-}" ] || echo "Retry-After: $RATE_RETRY_AFTER"
      echo
      echo "gh: ${RATE_MESSAGE:-API rate limit exceeded for installation} (HTTP ${RATE_STATUS:-403})" >&2
      exit 1
    fi
    ;;
esac
if [[ " $* " == *' --include '* ]]; then
  printf 'HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n'
fi
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

for endpoint in commit checks; do
  rm -f "$stub_dir/rate-seen"
  if ! RATE_ENDPOINT="$endpoint" replay "rate-$endpoint" "cd-qualification=success frontend=success backend=success"; then
    echo "rate-$endpoint: recorder must retry the rate-limited request" >&2
    exit 1
  fi
  expect "rate-$endpoint" backend success 1
done

rm -f "$stub_dir/rate-seen"
RATE_ENDPOINT=commit RATE_STATUS=429 RATE_RETRY_AFTER=0 replay rate-429 "cd-qualification=success frontend=success backend=success"
expect rate-429 backend success 1

rm -f "$stub_dir/rate-seen"
RATE_ENDPOINT=commit RATE_REMAINING=1 RATE_RETRY_AFTER=0 replay secondary-rate "cd-qualification=success frontend=success backend=success"
expect secondary-rate backend success 1

rm -f "$stub_dir/rate-seen"
if RATE_ENDPOINT=commit RATE_REMAINING=1 RATE_MESSAGE='Resource not accessible by integration' \
  replay permission-denied "cd-qualification=success frontend=success backend=success"; then
  echo 'permission-denied: a normal 403 must not be retried' >&2
  exit 1
fi

rm -f "$stub_dir/rate-seen"
if RATE_ENDPOINT=commit RATE_RESET_OFFSET=3600 WAIT=0 \
  replay rate-deadline "cd-qualification=success frontend=success backend=success"; then
  echo 'rate-deadline: a reset outside the deadline must fail closed' >&2
  exit 1
fi

echo "record-checks fixtures passed"
