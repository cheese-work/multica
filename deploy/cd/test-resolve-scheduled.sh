#!/usr/bin/env bash
# CHE-5182: a scheduled cd-deploy run deploys the newest qualified main commit,
# or ends with "nothing new to deploy" when C00 already runs it (or a descendant).
# Stub `gh` replays the run list and the compare API; stub `curl` is C00's config.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin"
cat >"$tmp_dir/bin/gh" <<'STUB'
#!/usr/bin/env bash
if [ "$1" = run ]; then
  [ -z "${STUB_QUALIFIED:-}" ] || echo "$STUB_QUALIFIED"
  exit 0
fi
printf 'HTTP/2.0 200 OK\r\n\r\n'
case "$2" in
  */git/ref/heads/main*) echo "$STUB_MAIN" ;;
  */compare/*) echo "$STUB_COMPARE" ;;
  *) echo "unexpected gh api $2" >&2; exit 1 ;;
esac
STUB
cat >"$tmp_dir/bin/curl" <<'STUB'
#!/usr/bin/env bash
echo "{\"server_commit\":\"$STUB_LIVE\"}"
STUB
chmod +x "$tmp_dir/bin/gh" "$tmp_dir/bin/curl"

live="1111111111111111111111111111111111111111"
newer="2222222222222222222222222222222222222222"

# run_case <name> <qualified sha> <main sha> <compare status>; live is always $live
run_case() {
  : >"$tmp_dir/out"; : >"$tmp_dir/summary"
  status=0
  PATH="$tmp_dir/bin:$PATH" STUB_QUALIFIED="$2" STUB_MAIN="$3" STUB_COMPARE="$4" STUB_LIVE="$live" \
    GH_TOKEN=x GITHUB_REPOSITORY=cheese-work/multica C00_CONFIG_URL=https://c00.invalid/config \
    GITHUB_OUTPUT="$tmp_dir/out" GITHUB_STEP_SUMMARY="$tmp_dir/summary" \
    bash deploy/cd/resolve-scheduled.sh >"$tmp_dir/log" 2>&1 || status=$?
  echo "case: $1 (exit $status)"
}
fail() { echo "FAIL: $*" >&2; cat "$tmp_dir/log" >&2; exit 1; }
nothing_new() {
  [ "$status" = 0 ] || fail "expected success"
  grep -qx "sha=" "$tmp_dir/out" || fail "sha must be empty"
  grep -q "nothing new to deploy" "$tmp_dir/summary" || fail "summary must say nothing new to deploy"
}

run_case "newer qualified commit is deployed" "$newer" "$newer" ahead
[ "$status" = 0 ] || fail "expected success"
grep -qx "sha=$newer" "$tmp_dir/out" || fail "sha output missing"
! grep -q "nothing new" "$tmp_dir/summary" || fail "must not say nothing new"

run_case "C00 already runs the newest qualified commit" "$live" "$live" ""
nothing_new

run_case "newest qualified commit is behind C00" "$newer" "$newer" behind
nothing_new

run_case "no qualified main commit" "" "$newer" ""
nothing_new

echo "OK"
