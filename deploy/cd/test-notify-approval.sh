#!/usr/bin/env bash
set -euo pipefail

# notify-approval.sh (CHE-2418 B5, CHE-4879): one notice per waiting approval, by
# webhook and/or a Multica comment. No route configured, or no route delivered,
# fails the job. curl is mocked; it records its arguments and can be told to fail.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir -p "$work_dir/bin"

cat >"$work_dir/bin/curl" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$NOTIFY_TEST_DIR/curl-args"
[ ! -f "$NOTIFY_TEST_DIR/curl-fails" ] || exit 22
case "$*" in
  *"-d @-"*) cat >"$NOTIFY_TEST_DIR/curl-body"; printf '{"id":"new"}' ;;
  *"/api/me"*) printf '{"id":"bot-1"}' ;;
  *) cat "$NOTIFY_TEST_DIR/list.json" 2>/dev/null || echo '[]' ;;
esac
MOCK
chmod +x "$work_dir/bin/curl"
# gh mock: `gh api repos/R/compare/<ask>...<ours> -q .status` answers from $NOTIFY_TEST_DIR/gh-status.
cat >"$work_dir/bin/gh" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$NOTIFY_TEST_DIR/gh-args"
[ ! -f "$NOTIFY_TEST_DIR/gh-fails" ] || exit 1
cat "$NOTIFY_TEST_DIR/gh-status" 2>/dev/null || echo ahead
MOCK
chmod +x "$work_dir/bin/gh"
export PATH="$work_dir/bin:$PATH" NOTIFY_TEST_DIR="$work_dir"
export SOURCE_SHA=0123456789abcdef0123456789abcdef01234567
export RUN_URL=https://github.com/cheese-work/multica/actions/runs/42
export WEBHOOK_URL=https://ntfy.example/topic
unset MULTICA_TOKEN MULTICA_SERVER_URL MULTICA_WORKSPACE_ID MULTICA_NOTIFY_ISSUE MULTICA_NOTIFY_MEMBER_ID

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

# No route at all (CHE-4879): the job goes red, nothing is sent.
rm -f "$work_dir/curl-args"
if output="$(WEBHOOK_URL= run)"; then echo "no route: want non-zero exit" >&2; exit 1; fi
[[ "$output" == *"::error"* ]] || { echo "no route: no error: $output" >&2; exit 1; }
[ ! -e "$work_dir/curl-args" ] || { echo "no route: curl was called" >&2; exit 1; }

# Multica route: one comment to Cheese with the SHA and the dispatch one-liner.
export MULTICA_TOKEN=mul_secret_token MULTICA_SERVER_URL=https://multica.example MULTICA_WORKSPACE_ID=ws-1
export MULTICA_NOTIFY_ISSUE=issue-1 MULTICA_NOTIFY_MEMBER_ID=member-1
rm -f "$work_dir/curl-args" "$work_dir/curl-body"
output="$(WEBHOOK_URL= run)" || { echo "multica: want exit 0" >&2; echo "$output" >&2; exit 1; }
args="$(cat "$work_dir/curl-args")"
[[ "$args" == *"https://multica.example/api/issues/issue-1/comments"* ]] || { echo "multica: wrong url: $args" >&2; exit 1; }
[[ "$args" != *"$MULTICA_TOKEN"* ]] || { echo "token must not be in argv: $args" >&2; exit 1; }
body="$(cat "$work_dir/curl-body")"
python3 - "$body" "$SOURCE_SHA" <<'PY' || { echo "multica body wrong: $body" >&2; exit 1; }
import json, sys
b = json.loads(sys.argv[1]); sha = sys.argv[2]
c = b["content"]
assert "mention://member/member-1" in c
assert f"gh workflow run cd-deploy.yml --repo cheese-work/multica --ref main -f candidate_sha={sha}" in c
assert not b.get("supersedes")
PY

# Asks are authenticated by author: a marker comment from anyone but the token's own
# user is ignored, so it can neither suppress the notice nor be superseded.
OLD=ffffffffffffffffffffffffffffffffffffffff
ask() { # id author_id sha [resolved_at]
  printf '{"id":"%s","author_type":"member","author_id":"%s","content":"candidate_sha=%s <!-- cd-deploy-candidate -->","created_at":"2026-11-09T01:00:00Z","resolved_at":%s,"superseded_by_id":null}' \
    "$1" "$2" "$3" "${4:-null}"
}
list() { printf '[%s]' "$(IFS=,; echo "$*")" >"$work_dir/list.json"; }
post() { rm -f "$work_dir/curl-args" "$work_dir/curl-body"; WEBHOOK_URL= run >/dev/null; }

# Forged marker for our own SHA from another author: the real notice is still sent.
list "$(ask forged someone-else "$SOURCE_SHA")"
post
[ -e "$work_dir/curl-body" ] || { echo "forged marker suppressed the notice" >&2; exit 1; }

# A newer SHA supersedes the older open ask of ours (gh compare says ahead);
# a resolved ask and a forged ask are not superseded.
list "$(ask old-ask bot-1 "$OLD")" "$(ask done-ask bot-1 "$OLD" '"2026-11-09T03:00:00Z"')" "$(ask forged someone-else "$OLD")"
echo ahead >"$work_dir/gh-status"
post
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["supersedes"]==["old-ask"]' "$work_dir/curl-body" \
  || { echo "newer SHA must supersede only old-ask: $(cat "$work_dir/curl-body")" >&2; exit 1; }

# An older candidate finishing after a newer one must not replace the newer ask.
for status in behind identical; do
  list "$(ask newer-ask bot-1 "$OLD")"
  echo "$status" >"$work_dir/gh-status"
  post
  [ ! -e "$work_dir/curl-body" ] || { echo "$status: stale candidate posted over the newer ask" >&2; exit 1; }
done

# The same SHA is not asked twice; no ancestry lookup is needed for it.
list "$(ask same bot-1 "$SOURCE_SHA")"
rm -f "$work_dir/gh-args"
post
[ ! -e "$work_dir/curl-body" ] || { echo "same SHA asked twice" >&2; exit 1; }

# An ancestry lookup that fails or answers diverged fails the job; nothing is posted.
list "$(ask old-ask bot-1 "$OLD")"
echo diverged >"$work_dir/gh-status"
rm -f "$work_dir/curl-body"
if WEBHOOK_URL= run >/dev/null; then echo "diverged: want non-zero exit" >&2; exit 1; fi
[ ! -e "$work_dir/curl-body" ] || { echo "diverged: comment posted" >&2; exit 1; }
touch "$work_dir/gh-fails"
if WEBHOOK_URL= run >/dev/null; then echo "gh failure: want non-zero exit" >&2; exit 1; fi
rm -f "$work_dir/gh-fails" "$work_dir/list.json" "$work_dir/gh-status"

# RUN_LINK, when set, is the run link in the Multica comment.
RUN_LINK=https://github.com/cheese-work/multica/actions/runs/777 post
grep -q 'actions/runs/777' "$work_dir/curl-body" || { echo "RUN_LINK missing from comment" >&2; exit 1; }

# Both routes configured: both fire. A failing Multica route alone fails the job
# even though the webhook delivered nothing it could report.
rm -f "$work_dir/curl-args"
run >/dev/null
[ "$(wc -l <"$work_dir/curl-args")" -eq 4 ] || { echo "both routes: want webhook POST + me + list + comment" >&2; cat "$work_dir/curl-args" >&2; exit 1; }
touch "$work_dir/curl-fails"
if run >/dev/null; then echo "both routes failing: want non-zero exit" >&2; exit 1; fi
rm "$work_dir/curl-fails"
unset MULTICA_TOKEN MULTICA_SERVER_URL MULTICA_WORKSPACE_ID MULTICA_NOTIFY_ISSUE MULTICA_NOTIFY_MEMBER_ID

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
