#!/usr/bin/env bash
# CHE-5208: run `multica guard check-pr` on one PR. Fails closed on a half-set policy: a policy with no digest, or a
# digest with no policy, was not judged, so the check is red (exit 2). CHE-5934: with neither variable set there is no
# stored policy to copy, so the CLI runs on its compiled default policy, as the daemon's guard does
# (guard.PolicyFromEnv); the PR is still judged. The CLI verifies a set policy against the digest variable and refuses on
# a mismatch; this script never computes the digest.
# A merge-queue build has no PR payload: the PR comes from the queue branch name
# (refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>, set by GitHub in MERGE_GROUP_HEAD_REF). Anything
# else is unreadable, so the PR was not judged and the check is red.
# Env: PR_URL or MERGE_GROUP_HEAD_REF, REPO, GUARD_POLICY_JSON, GUARD_POLICY_SHA256, RUNNER_TEMP; MULTICA_BIN (default
# $RUNNER_TEMP/multica); GH_TOKEN for gh.
set -euo pipefail

policy_set=1
if [ -z "${GUARD_POLICY_JSON:-}" ] && [ -z "${GUARD_POLICY_SHA256:-}" ]; then
  policy_set=0
  echo "::notice::PR guard rules: GUARD_POLICY_JSON and GUARD_POLICY_SHA256 are unset, so no stored policy is copied here; judging on the compiled default policy."
elif [ -z "${GUARD_POLICY_JSON:-}" ]; then
  echo "::error::PR guard rules: repository variable GUARD_POLICY_JSON is not set but GUARD_POLICY_SHA256 is, so the review policy is unknown and the PR cannot be judged. Set GUARD_POLICY_JSON to the stored guard policy JSON, or unset both."
  exit 2
elif [ -z "${GUARD_POLICY_SHA256:-}" ]; then
  echo "::error::PR guard rules: repository variable GUARD_POLICY_SHA256 is not set; the policy has no digest. Set it to the sha256 of GUARD_POLICY_JSON, or unset both."
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

if [ "$policy_set" = 1 ]; then
  policy="$RUNNER_TEMP/guard-policy.json"
  printf '%s' "$GUARD_POLICY_JSON" > "$policy"
  export MULTICA_GUARD_POLICY="$policy"
  export MULTICA_GUARD_POLICY_SHA256="$GUARD_POLICY_SHA256"
fi
MULTICA_GUARD_REAL_GH="$(command -v gh || true)"
export MULTICA_GUARD_REAL_GH

exec "${MULTICA_BIN:-$RUNNER_TEMP/multica}" guard check-pr "$pr" -R "$REPO"
