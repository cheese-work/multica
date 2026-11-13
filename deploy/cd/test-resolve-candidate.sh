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
# Replays canned answers. The qualified-run query records its args and prints
# the canned headSha (empty = no qualified commit).
case "$*" in
  *"--status success"*) echo "$*" >"$STUB_DIR/gh-args"; [ -z "${STUB_SHA:-}" ] || echo "$STUB_SHA" ;;
  "run list"*) echo "${STUB_NEWEST_RUN:-}" ;;
  *compare/*) echo "${STUB_BEHIND:-}" ;;
  *commits/main*) echo "${STUB_MAIN:-}" ;;
  *commits/*) echo "${STUB_CAND_DATE:-}" ;;
esac
STUB
cat >"$tmp_dir/bin/curl" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$STUB_DIR/curl-args"
cat >/dev/null
exit "${STUB_CURL_EXIT:-0}"
STUB
chmod +x "$tmp_dir/bin/curl"
chmod +x "$tmp_dir/bin/gh"

newest="4f9e44d6b8aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
typed="1111111111111111111111111111111111111111"

# run_case <name> <STUB_SHA> <CANDIDATE_SHA> <INPUT_MANIFEST> -> sets status, out, summary
run_case() {
  : >"$tmp_dir/out"; : >"$tmp_dir/summary"; rm -f "$tmp_dir/gh-args" "$tmp_dir/curl-args"
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

# CHE-5352: a candidate behind main says so. Dates are UTC; the notice prints ICT.
cand_date="2026-11-12T03:04:00Z"                     # 10:04 ICT
main_fresh="mainsha0000000000000000000000000000000000	2026-11-12T04:00:00Z"
main_late="mainsha0000000000000000000000000000000000	2026-11-12T09:27:00Z"
run="mainsha0000000000000000000000000000000000 https://github.com/cheese-work/multica/actions/runs/43191000000"
notice_env() { # <main "sha<TAB>date"> <behind> [multica server url]
  export STUB_NEWEST_RUN="$run" STUB_CAND_DATE="$cand_date" STUB_MAIN="$1" STUB_BEHIND="$2"
  export MULTICA_SERVER_URL="${3:-}" MULTICA_TOKEN="${3:+tok}" MULTICA_NOTICE_ISSUE_ID="${3:+issue-4960}"
}

notice_env "$main_fresh" 0
run_case "candidate is main HEAD: no notice" "$newest" "" ""
[ "$status" = 0 ] || fail "expected success"
! grep -q "behind main" "$tmp_dir/summary" || fail "no notice when the candidate is main HEAD"

notice_env "$main_fresh" 3
run_case "slightly behind: summary notice only" "$newest" "" ""
[ "$status" = 0 ] || fail "expected success"
grep -qx "sha=$newest" "$tmp_dir/out" || fail "deployed sha must not change"
grep -q "Deploying $newest (12 Nov 2026 10:04 ICT), 3 commits behind main. Newest main commit failed or has not run qualification: https://github.com/cheese-work/multica/actions/runs/43191000000." "$tmp_dir/summary" || fail "summary notice missing or wrong"
! grep -q '::warning' "$tmp_dir/log" || fail "no warning at 3 commits / under 2 h"
! grep -q '^notice=' "$tmp_dir/out" || fail "no post text at 3 commits / under 2 h"

notice_env "$main_fresh" 145 "https://multica.example"
run_case "far behind by count: warning and one post" "$newest" "" ""
[ "$status" = 0 ] || fail "expected success"
grep -q "145 commits behind main" "$tmp_dir/summary" || fail "count missing from summary"
grep -q '::warning' "$tmp_dir/log" || fail "expected ::warning"
grep -q '^notice=Deploying .*145 commits behind main' "$tmp_dir/out" || fail "warning case must hand the notice text to the post job"
[ ! -e "$tmp_dir/curl-args" ] || fail "resolve-candidate runs on a hosted runner and must not post"

notice_env "$main_late" 5 "https://multica.example"
run_case "far behind by time: warning" "$newest" "" ""
grep -q '::warning' "$tmp_dir/log" || fail "expected ::warning when more than 2 h behind"

notice_env "$main_fresh" 145
run_case "far behind: warning, still succeeds" "$newest" "" ""
[ "$status" = 0 ] || fail "expected success"
grep -q '::warning' "$tmp_dir/log" || fail "expected ::warning"

notice_env "" ""
run_case "lookup failure never blocks the deploy" "$newest" "" ""
[ "$status" = 0 ] || fail "expected success"
grep -qx "sha=$newest" "$tmp_dir/out" || fail "sha must still be emitted"

notice_env "$main_fresh" 145 "https://multica.example"
run_case "typed SHA gets no notice" "" "$typed" ""
[ ! -s "$tmp_dir/summary" ] || fail "typed SHA must not be annotated"

# post-stale-notice.sh runs on X99 (tailnet) and posts the handed-over text once.
# post_case <NOTICE> <server> <token> <curl exit> -> sets status
post_case() {
  rm -f "$tmp_dir/curl-args"; status=0
  PATH="$tmp_dir/bin:$PATH" STUB_DIR="$tmp_dir" STUB_CURL_EXIT="$4" NOTICE="$1" \
    MULTICA_SERVER_URL="$2" MULTICA_TOKEN="$3" MULTICA_WORKSPACE_ID=ws MULTICA_NOTICE_ISSUE_ID=issue-4960 \
    bash deploy/cd/post-stale-notice.sh >"$tmp_dir/log" 2>&1 || status=$?
  echo "post case: $1 (exit $status)"
}
post_case "Deploying abc, 145 commits behind main." "https://multica.example" tok 0
[ "$status" = 0 ] || fail "expected success"
[ "$(wc -l <"$tmp_dir/curl-args")" = 1 ] || fail "expected exactly one post"
grep -q "multica.example/api/issues/issue-4960/comments" "$tmp_dir/curl-args" || fail "post must target the notice issue"
! grep -q "tok" "$tmp_dir/curl-args" || fail "token must not appear in argv"

post_case "" "https://multica.example" tok 0
[ "$status" = 0 ] && [ ! -e "$tmp_dir/curl-args" ] || fail "empty notice must post nothing"

post_case "Deploying abc, 145 commits behind main." "https://multica.example" "" 0
[ "$status" = 0 ] && [ ! -e "$tmp_dir/curl-args" ] || fail "no token: skip, no failure"
grep -q '::warning' "$tmp_dir/log" || fail "no token: expected ::warning"

post_case "Deploying abc, 145 commits behind main." "https://multica.example" tok 22
[ "$status" = 0 ] || fail "a failed post must not fail the job"
grep -q '::warning' "$tmp_dir/log" || fail "failed post: expected ::warning"

echo "OK"
