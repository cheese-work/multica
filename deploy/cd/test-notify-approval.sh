#!/usr/bin/env bash
set -euo pipefail

# notify-approval.sh (CHE-2418 B5, CHE-4879): one notice per waiting approval, by
# webhook and/or a Multica decision card (POST /api/decisions/ci). No route
# configured only warns (the deploy gate does not depend on the notice); a
# configured route that delivers nothing fails the job. curl and gh are mocked; they
# record their arguments and can be told to fail.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir -p "$work_dir/bin"

cat >"$work_dir/bin/curl" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$NOTIFY_TEST_DIR/curl-args"
[ ! -f "$NOTIFY_TEST_DIR/curl-fails" ] || exit 22
out="" fmt=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in -o) out="${args[i+1]}" ;; -w) fmt="${args[i+1]}" ;; esac
done
case "$*" in
  *"/api/decisions/ci"*)
    cat >"$NOTIFY_TEST_DIR/curl-body"
    [ -z "$out" ] || printf '{"id":"card-1"}' >"$out"
    [ -z "$fmt" ] || cat "$NOTIFY_TEST_DIR/ci-status" 2>/dev/null || printf 201 ;;
  *"/api/"*) exit 22 ;; # a scoped decisions:ci_request PAT gets 403 on every other route
  *"-d @-"*) cat >"$NOTIFY_TEST_DIR/curl-body"; printf '{"id":"new"}' ;;
esac
MOCK
chmod +x "$work_dir/bin/curl"
# gh mock: `gh api repos/R/commits/<sha> -q .commit.committer.date` answers the commit time.
cat >"$work_dir/bin/gh" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$NOTIFY_TEST_DIR/gh-args"
[ ! -f "$NOTIFY_TEST_DIR/gh-fails" ] || exit 1
echo 2026-11-11T09:00:00Z
MOCK
chmod +x "$work_dir/bin/gh"
export PATH="$work_dir/bin:$PATH" NOTIFY_TEST_DIR="$work_dir"
export SOURCE_SHA=0123456789abcdef0123456789abcdef01234567
export RUN_URL=https://github.com/cheese-work/multica/actions/runs/42
export WEBHOOK_URL=https://ntfy.example/topic
unset MULTICA_TOKEN MULTICA_SERVER_URL MULTICA_WORKSPACE_ID MULTICA_NOTIFY_ISSUE MULTICA_NOTIFY_MEMBER_ID MULTICA_NOTIFY_SOURCE_COMMENT

run() { bash deploy/cd/notify-approval.sh 2>&1; }

output="$(run)" || { echo "configured: want exit 0" >&2; echo "$output" >&2; exit 1; }
[ "$(wc -l <"$work_dir/curl-args")" -eq 1 ] || { echo "want exactly one POST" >&2; exit 1; }
args="$(cat "$work_dir/curl-args")"
[[ "$args" == *"$WEBHOOK_URL"* ]] || { echo "webhook url not used: $args" >&2; exit 1; }
[[ "$args" != *"$SOURCE_SHA"* ]] || { echo "message must travel in the body, not argv: $args" >&2; exit 1; }
body="$(cat "$work_dir/curl-body")"
[[ "$body" == *"0123456789ab"* ]] || { echo "version missing from body: $body" >&2; exit 1; }
[[ "$body" == *"candidate_sha=$SOURCE_SHA"* ]] || { echo "full candidate missing from body: $body" >&2; exit 1; }
[[ "$body" == *"$RUN_URL"* ]] || { echo "dispatch link missing from body: $body" >&2; exit 1; }
python3 -c 'import json,sys; json.loads(sys.argv[1])' "$body" || { echo "body is not JSON: $body" >&2; exit 1; }

# A failing webhook with no other route fails the job.
touch "$work_dir/curl-fails"
if output="$(run)"; then echo "failing webhook: want non-zero exit" >&2; exit 1; fi
[[ "$output" == *"::error"* ]] || { echo "failing webhook: no error: $output" >&2; exit 1; }
rm "$work_dir/curl-fails"

# No route at all: a warning, exit 0, nothing sent. Every scheduled run used to
# turn red for a missing secret, not for a deploy fault.
rm -f "$work_dir/curl-args"
output="$(WEBHOOK_URL= run)" || { echo "no route: want exit 0: $output" >&2; exit 1; }
[[ "$output" == *"::warning"* && "$output" != *"::error"* ]] || { echo "no route: want warning only: $output" >&2; exit 1; }
[ ! -e "$work_dir/curl-args" ] || { echo "no route: curl was called" >&2; exit 1; }

# Multica route: one decision card for Cheese with the SHA and the dispatch one-liner.
export MULTICA_TOKEN=mul_secret_token MULTICA_SERVER_URL=https://multica.example MULTICA_WORKSPACE_ID=ws-1
export MULTICA_NOTIFY_ISSUE=issue-1 MULTICA_NOTIFY_MEMBER_ID=member-1
SRC=11111111-1111-4111-8111-111111111111
export MULTICA_NOTIFY_SOURCE_COMMENT=$SRC
post() { rm -f "$work_dir/curl-args" "$work_dir/curl-body"; WEBHOOK_URL= run >/dev/null; }
rm -f "$work_dir/curl-args" "$work_dir/curl-body"
output="$(WEBHOOK_URL= run)" || { echo "multica: want exit 0" >&2; echo "$output" >&2; exit 1; }
args="$(cat "$work_dir/curl-args")"
[[ "$args" == *"https://multica.example/api/decisions/ci"* ]] || { echo "multica: wrong url: $args" >&2; exit 1; }
[[ "$args" != *"$MULTICA_TOKEN"* ]] || { echo "token must not be in argv: $args" >&2; exit 1; }
body="$(cat "$work_dir/curl-body")"
python3 - "$body" "$SOURCE_SHA" "$SRC" <<'PY' || { echo "multica body wrong: $body" >&2; exit 1; }
import json, sys
b = json.loads(sys.argv[1]); sha = sys.argv[2]; src = sys.argv[3]
assert b["issue_id"] == "issue-1" and b["for"] == "member-1", b
assert b["candidate_sha"] == sha and b["candidate_at"] == "2026-11-11T09:00:00Z", b
assert set(b) == {"issue_id", "for", "ask", "candidate_sha", "candidate_at"}, sorted(b)
a = b["ask"]
assert a["source_issue_id"] == "issue-1" and a["source_comment_id"] == src, a
assert len(a["options"]) >= 2 and a["recommended_option"] in [o["label"] for o in a["options"]], a
text = json.dumps(a)
assert f"gh workflow run cd-deploy.yml --repo cheese-work/multica --ref main -f candidate_sha={sha}" in text
assert sha[:12] in text
PY

# The server decides freshness: a new card (201), a replay (200) and a newer
# candidate already asked (409) are all delivered; any other status fails.
for code in 201 200 409; do
  printf %s "$code" >"$work_dir/ci-status"; post
done
for code in 400 403 500; do
  printf %s "$code" >"$work_dir/ci-status"
  if WEBHOOK_URL= run >/dev/null; then echo "status $code: want non-zero exit" >&2; exit 1; fi
done
rm -f "$work_dir/ci-status"
# No anchor comment configured: the Multica route is not configured, so the job only warns.
output="$(WEBHOOK_URL= MULTICA_NOTIFY_SOURCE_COMMENT= run)" || { echo "no anchor comment: want exit 0: $output" >&2; exit 1; }
[[ "$output" == *"::warning"* && "$output" != *"::error"* ]] || { echo "no anchor comment: want warning only: $output" >&2; exit 1; }
touch "$work_dir/gh-fails"
if WEBHOOK_URL= run >/dev/null; then echo "commit lookup failure: want non-zero exit" >&2; exit 1; fi
rm -f "$work_dir/gh-fails"

# RUN_LINK, when set, is the run link in the card.
RUN_LINK=https://github.com/cheese-work/multica/actions/runs/777 post
grep -q 'actions/runs/777' "$work_dir/curl-body" || { echo "RUN_LINK missing from card" >&2; exit 1; }

# Both routes configured: both fire. A failing Multica route alone fails the job
# even though the webhook delivered nothing it could report.
rm -f "$work_dir/curl-args"
run >/dev/null
[ "$(wc -l <"$work_dir/curl-args")" -eq 2 ] || { echo "both routes: want webhook POST + card" >&2; cat "$work_dir/curl-args" >&2; exit 1; }
touch "$work_dir/curl-fails"
if run >/dev/null; then echo "both routes failing: want non-zero exit" >&2; exit 1; fi
rm "$work_dir/curl-fails"
unset MULTICA_TOKEN MULTICA_SERVER_URL MULTICA_WORKSPACE_ID MULTICA_NOTIFY_ISSUE MULTICA_NOTIFY_MEMBER_ID MULTICA_NOTIFY_SOURCE_COMMENT

# A manual run has no workflow_run payload: SOURCE_SHA comes from INPUT_REF.
# Review round 2: an annotated tag must resolve to its commit, not the tag
# object. Covers branch, annotated tag, lightweight tag and a full commit SHA.
git_quiet() { git -c user.name=t -c user.email=t@example.com "$@"; }
origin="$work_dir/origin.git"
src="$work_dir/src"
git init -q --bare "$origin"
git -C "$origin" config uploadpack.allowAnySHA1InWant true
git init -q -b main "$src"
git_quiet -C "$src" commit -q --allow-empty -m one
git_quiet -C "$src" tag -a v-annotated -m "annotated"
git_quiet -C "$src" tag v-light
git_quiet -C "$src" branch topic
git_quiet -C "$src" push -q "file://$origin" --all
git_quiet -C "$src" push -q "file://$origin" --tags
commit="$(git -C "$src" rev-parse HEAD)"
tag_object="$(git -C "$src" rev-parse v-annotated)"
[ "$tag_object" != "$commit" ] || { echo "fixture: annotated tag must differ from its commit" >&2; exit 1; }
clone="$work_dir/clone"
git init -q "$clone"
git -C "$clone" remote add origin "file://$origin"

for ref in main topic v-annotated v-light "$commit"; do
  rm -f "$work_dir/curl-args" "$work_dir/curl-body"
  output="$(cd "$clone" && SOURCE_SHA= INPUT_REF="$ref" bash "$root_dir/deploy/cd/notify-approval.sh" 2>&1)" \
    || { echo "INPUT_REF=$ref: want exit 0" >&2; echo "$output" >&2; exit 1; }
  body="$(cat "$work_dir/curl-body")"
  [[ "$body" == *"${commit:0:12}"* ]] || { echo "INPUT_REF=$ref: want commit ${commit:0:12} in body: $body" >&2; exit 1; }
done

# An unresolvable ref warns and sends nothing; it never fails the job.
rm -f "$work_dir/curl-args"
output="$(cd "$clone" && SOURCE_SHA= INPUT_REF=no-such-ref bash "$root_dir/deploy/cd/notify-approval.sh" 2>&1)" \
  || { echo "bad ref: want exit 0" >&2; echo "$output" >&2; exit 1; }
[[ "$output" == *"::warning"* ]] || { echo "bad ref: no warning: $output" >&2; exit 1; }
[ ! -e "$work_dir/curl-args" ] || { echo "bad ref: curl was called" >&2; exit 1; }

echo "test-notify-approval.sh: PASS"
