#!/usr/bin/env bash
set -euo pipefail

source_sha=$1
checks_path=$2

echo 'eligible=false' >> "$GITHUB_OUTPUT"
printf '## C00 deploy candidate `%s`\n\n' "$source_sha" >> "$GITHUB_STEP_SUMMARY"

require_current() {
  local result
  result="$(GITHUB_OUTPUT=/dev/stdout bash deploy/cd/require-current.sh "$source_sha")"
  [ "$result" = 'current=true' ] || exit 0
}

require_current
bash deploy/cd/record-checks.sh "$source_sha" "$checks_path"
require_current

if ! node deploy/cd/admission.mjs checks --checks "$checks_path" --source-sha "$source_sha"; then
  printf 'Skipped before approval: required checks for `%s` are not admissible. See the preflight log.\n' \
    "$source_sha" >> "$GITHUB_STEP_SUMMARY"
  exit 0
fi

echo 'eligible=true' >> "$GITHUB_OUTPUT"
printf 'Candidate `%s` moves forward from C00 and its required checks passed; requesting C00 approval.\n' \
  "$source_sha" >> "$GITHUB_STEP_SUMMARY"
