#!/usr/bin/env bash
# Writes current=true|false: whether <source_sha> is still the commit to deploy.
#
# It is superseded by a newer main commit only if candidate-preflight would
# admit that commit (or its required checks are still running). A newer commit
# whose required checks have settled without passing can never deploy, so it
# must not replace an approved candidate that can (CHE-2027). A candidate that
# is not in main's history is never current.
# A superseded run is skipped, never failed; an API error fails closed.
#
# usage: require-current.sh <source-sha>
# env:   GITHUB_REPOSITORY, GH_TOKEN, GITHUB_OUTPUT, GITHUB_STEP_SUMMARY
# needs: node and PyYAML (ensure-pyyaml.sh), as record-checks.sh does
set -euo pipefail

source_sha=$1

superseded_by() {
  echo 'current=false' >> "$GITHUB_OUTPUT"
  printf 'Skipped before C00 mutation: candidate `%s` is superseded by main `%s`.\n' \
    "$source_sha" "$1" >> "$GITHUB_STEP_SUMMARY"
  exit 0
}

main_sha="$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/main" --jq '.object.sha')"
if [ "$source_sha" = "$main_sha" ]; then
  echo 'current=true' >> "$GITHUB_OUTPUT"
  exit 0
fi

compare="repos/${GITHUB_REPOSITORY}/compare/${source_sha}...${main_sha}"
status="$(gh api "${compare}?per_page=1" --jq '.status')"
[ "$status" = "ahead" ] || superseded_by "$main_sha"

# Oldest first; the API stops at 250 commits, too far behind to trust.
# Read into a variable first: mapfile would hide a failed or partial listing.
history="$(gh api "${compare}?per_page=100" --paginate --jq '.commits[].sha')"
mapfile -t newer <<< "$history"
[ "${#newer[@]}" -lt 250 ] || superseded_by "$main_sha"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

for (( i = ${#newer[@]} - 1; i >= 0; i-- )); do
  sha="${newer[$i]}"
  # One read, no waiting: running checks end it as "pending", which counts.
  CHECKS_WAIT_SECONDS=0 bash deploy/cd/record-checks.sh "$sha" "$work_dir/checks.json" > /dev/null
  pending="$(node deploy/cd/admission.mjs pending --checks "$work_dir/checks.json")"
  if [ -n "$pending" ] \
    || node deploy/cd/admission.mjs checks --checks "$work_dir/checks.json" --source-sha "$sha" > /dev/null 2>&1; then
    superseded_by "$sha"
  fi
  printf 'Main commit `%s` cannot deploy (required checks settled without passing); not superseding.\n' \
    "$sha" >> "$GITHUB_STEP_SUMMARY"
done

echo 'current=true' >> "$GITHUB_OUTPUT"
