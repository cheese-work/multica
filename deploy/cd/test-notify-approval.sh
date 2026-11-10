#!/usr/bin/env bash
set -euo pipefail

# notify-approval.sh (CHE-2418 B5): one POST per waiting approval, carrying the
# version and the run link; no webhook configured is a skip, never a failure.
# curl is mocked; it records its arguments and can be told to fail.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir -p "$work_dir/bin"

cat >"$work_dir/bin/curl" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$NOTIFY_TEST_DIR/curl-args"
cat >"$NOTIFY_TEST_DIR/curl-body"
[ ! -f "$NOTIFY_TEST_DIR/curl-fails" ]
MOCK
chmod +x "$work_dir/bin/curl"
export PATH="$work_dir/bin:$PATH" NOTIFY_TEST_DIR="$work_dir"
export SOURCE_SHA=0123456789abcdef0123456789abcdef01234567
export RUN_URL=https://github.com/cheese-work/multica/actions/runs/42
export WEBHOOK_URL=https://ntfy.example/topic

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

# A failing webhook warns but must not fail the job.
touch "$work_dir/curl-fails"
output="$(run)" || { echo "failing webhook: want exit 0" >&2; echo "$output" >&2; exit 1; }
[[ "$output" == *"::warning"* ]] || { echo "failing webhook: no warning: $output" >&2; exit 1; }
rm "$work_dir/curl-fails"

# No webhook: skip with a notice, send nothing.
rm -f "$work_dir/curl-args"
output="$(WEBHOOK_URL= run)" || { echo "unset: want exit 0" >&2; exit 1; }
[[ "$output" == *"::notice"* ]] || { echo "unset: no notice: $output" >&2; exit 1; }
[ ! -e "$work_dir/curl-args" ] || { echo "unset: curl was called" >&2; exit 1; }

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
