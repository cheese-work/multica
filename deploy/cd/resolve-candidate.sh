#!/usr/bin/env bash
# CHE-4998: resolve which commit a cd-deploy dispatch deploys; writes `sha=` to
# $GITHUB_OUTPUT. A typed CANDIDATE_SHA wins. With no SHA and no evidence path,
# pick the newest main commit whose "CD qualification build" push run succeeded
# (`gh run list` is newest first). Any evidence path set -> `sha=` stays empty
# and the manual-evidence flow runs unchanged. Admission, freshness and
# quiescence still gate the result downstream.
set -euo pipefail

sha="${CANDIDATE_SHA:-}"
if [ -z "$sha" ] && [ -z "${INPUT_MANIFEST:-}${INPUT_BASELINE:-}${INPUT_EVENT:-}${INPUT_CHECKS:-}${INPUT_PROVENANCE:-}" ]; then
  sha="$(gh run list --repo "$GITHUB_REPOSITORY" --workflow "CD qualification build" \
    --branch main --event push --status success --limit 1 \
    --json headSha --jq '.[0].headSha // empty')"
  [ -n "$sha" ] || { echo "::error::no successful main qualification run to deploy; pass candidate_sha or wait for a qualified main commit" >&2; exit 1; }
  echo "Empty dispatch: deploying the newest qualified main commit $sha" >&2
  echo "Empty dispatch resolved to the newest qualified main commit: \`$sha\`" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
fi
echo "sha=$sha" >>"$GITHUB_OUTPUT"
