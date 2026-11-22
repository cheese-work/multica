#!/usr/bin/env bash
# CHE-5182: first job of a scheduled cd-deploy run. Writes `sha=` to
# $GITHUB_OUTPUT: the newest main commit whose "CD qualification build" push run
# succeeded, or empty when there is nothing new to deploy (no qualified commit,
# or C00 already runs it or a descendant). Reads only the GitHub API and C00's
# public config URL, so it takes no C00 runner, lock or environment.
# Needs GH_TOKEN, GITHUB_REPOSITORY, C00_CONFIG_URL; run from a full-depth checkout.
set -euo pipefail

nothing_new() {
  echo "sha=" >>"$GITHUB_OUTPUT"
  echo "Scheduled run: nothing new to deploy ($1)." >&2
  echo "nothing new to deploy ($1)" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
  exit 0
}

sha="$(gh run list --repo "$GITHUB_REPOSITORY" --workflow "CD qualification build" \
  --branch main --event push --status success --limit 1 \
  --json headSha --jq '.[0].headSha // empty')"
[ -n "$sha" ] || nothing_new "no qualified main commit"

current_out="$(mktemp)"
trap 'rm -f "$current_out"' EXIT
GITHUB_OUTPUT="$current_out" bash deploy/cd/require-current.sh "$sha"
grep -qx 'current=true' "$current_out" || nothing_new "$sha is not ahead of C00"

echo "sha=$sha" >>"$GITHUB_OUTPUT"
echo "Scheduled run resolved the newest qualified main commit: \`$sha\`" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
