#!/usr/bin/env bash
# CHE-4998: an empty cd-deploy dispatch resolves to the newest main commit with
# a successful "CD qualification build" push run. Stub `gh` replays the run list.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin"
cat >"$tmp_dir/bin/gh" <<'STUB'
#!/usr/bin/env bash
# Records the args; prints the canned headSha (empty = no qualified commit).
echo "$*" >"$STUB_DIR/gh-args"
[ -z "${STUB_SHA:-}" ] || echo "$STUB_SHA"
STUB
chmod +x "$tmp_dir/bin/gh"

newest="4f9e44d6b8aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
typed="1111111111111111111111111111111111111111"

# run_case <name> <STUB_SHA> <CANDIDATE_SHA> <INPUT_MANIFEST> -> sets status, out, summary
run_case() {
  : >"$tmp_dir/out"; : >"$tmp_dir/summary"; rm -f "$tmp_dir/gh-args"
  status=0
  PATH="$tmp_dir/bin:$PATH" STUB_DIR="$tmp_dir" STUB_SHA="$2" CANDIDATE_SHA="$3" \
    INPUT_MANIFEST="$4" INPUT_BASELINE="" INPUT_EVENT="" INPUT_CHECKS="" INPUT_PROVENANCE="" \
    GH_TOKEN=x GITHUB_REPOSITORY=cheese-work/multica \
    GITHUB_OUTPUT="$tmp_dir/out" GITHUB_STEP_SUMMARY="$tmp_dir/summary" \
    bash deploy/cd/resolve-candidate.sh >"$tmp_dir/log" 2>&1 || status=$?
  echo "case: $1 (exit $status)"
}
fail() { echo "FAIL: $*" >&2; cat "$tmp_dir/log" >&2; exit 1; }

run_case "empty inputs pick newest qualified main" "$newest" "" ""
[ "$status" = 0 ] || fail "expected success"
grep -qx "sha=$newest" "$tmp_dir/out" || fail "sha output missing"
grep -q "$newest" "$tmp_dir/summary" || fail "job summary missing the SHA"
grep -q -- "--branch main" "$tmp_dir/gh-args" && grep -q -- "--event push" "$tmp_dir/gh-args" \
  && grep -q -- "--status success" "$tmp_dir/gh-args" && grep -q "CD qualification build" "$tmp_dir/gh-args" \
  || fail "query must be successful main push runs of the qualification workflow"

run_case "no qualified main commit fails clearly" "" "" ""
[ "$status" != 0 ] || fail "expected failure"
grep -q "no successful main qualification run" "$tmp_dir/log" || fail "unclear message"
! grep -q '^sha=' "$tmp_dir/out" || fail "must not emit a sha"

run_case "typed SHA passes through untouched" "$newest" "$typed" ""
[ "$status" = 0 ] || fail "expected success"
grep -qx "sha=$typed" "$tmp_dir/out" || fail "typed sha must win"
[ ! -e "$tmp_dir/gh-args" ] || fail "must not query when a SHA is typed"

run_case "evidence-path dispatch is left alone" "$newest" "" "m.json"
[ "$status" = 0 ] || fail "expected success"
grep -qx "sha=" "$tmp_dir/out" || fail "sha must stay empty"
[ ! -e "$tmp_dir/gh-args" ] || fail "must not query in evidence mode"

echo "OK"
