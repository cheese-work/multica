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
[[ "$body" == *"$RUN_URL"* ]] || { echo "approval link missing from body: $body" >&2; exit 1; }
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

echo "test-notify-approval.sh: PASS"
