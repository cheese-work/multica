#!/usr/bin/env bash
# CHE-5208: run `multica guard check-pr` on one PR. Fails closed: with no review policy, or no
# digest for it, the PR was not judged, so the check is red (exit 2). The CLI verifies the policy
# against the digest variable and refuses on a mismatch; this script never computes the digest.
# A merge-queue build has no PR payload: the PR comes from the queue branch name
# (refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>, set by GitHub in MERGE_GROUP_HEAD_REF). Anything
# else is unreadable, so the PR was not judged and the check is red.
# Env: PR_URL or MERGE_GROUP_HEAD_REF, REPO, GUARD_POLICY_JSON, GUARD_POLICY_SHA256, RUNNER_TEMP; MULTICA_BIN (default
# $RUNNER_TEMP/multica); GH_TOKEN for gh.
set -euo pipefail

if [ -z "${GUARD_POLICY_JSON:-}" ]; then
  echo "::error::PR guard rules: repository variable GUARD_POLICY_JSON is not set, so the review policy is unknown and the PR cannot be judged. Set it to the stored guard policy JSON."
  exit 2
fi
if [ -z "${GUARD_POLICY_SHA256:-}" ]; then
  echo "::error::PR guard rules: repository variable GUARD_POLICY_SHA256 is not set; the policy has no digest. Set it to the sha256 of GUARD_POLICY_JSON."
  exit 2
fi

pr="${PR_URL:-}"
if [ -z "$pr" ]; then
  if [[ "${MERGE_GROUP_HEAD_REF:-}" =~ /pr-([0-9]+)-[0-9a-f]{7,40}$ ]]; then
    pr="${GITHUB_SERVER_URL:-https://github.com}/$REPO/pull/${BASH_REMATCH[1]}"
  else
    echo "::error::PR guard rules: no PR URL and the merge-queue branch name is unreadable, so the PR cannot be judged."
    exit 2
  fi
fi

policy="$RUNNER_TEMP/guard-policy.json"
printf '%s' "$GUARD_POLICY_JSON" > "$policy"
export MULTICA_GUARD_POLICY="$policy"
export MULTICA_GUARD_POLICY_SHA256="$GUARD_POLICY_SHA256"
MULTICA_GUARD_REAL_GH="$(command -v gh || true)"
export MULTICA_GUARD_REAL_GH

exec "${MULTICA_BIN:-$RUNNER_TEMP/multica}" guard check-pr "$pr" -R "$REPO"
