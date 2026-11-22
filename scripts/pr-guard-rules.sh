#!/usr/bin/env bash
# CHE-5208: run `multica guard check-pr` on one PR. Fails closed on a half-set policy: a policy with no digest, or a
# digest with no policy, was not judged, so the check is red (exit 2). CHE-5934: with neither variable set there is no
# stored policy to copy, so the CLI runs on its compiled default policy, as the daemon's guard does
# (guard.PolicyFromEnv); the PR is still judged. The CLI verifies a set policy against the digest variable and refuses on
# a mismatch; this script never computes the digest.
# A merge-queue build has no PR payload: the PR comes from the queue branch name
# (refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>, set by GitHub in MERGE_GROUP_HEAD_REF). Anything
# else is unreadable, so the PR was not judged and the check is red.
# CHE-5752: the variables are a copy of the stored policy (workspace settings.guard_policy). With GUARD_POLICY_READ_TOKEN
# (a read-only token) the check reads the live stored policy through the CLI and fails closed (exit 2) when the copy
# differs (key order aside) or the live policy cannot be read, including a response with no settings object. GUARD_POLICY_LIVE_REQUIRED=1 makes a missing token red
# too; without either, the check notes that freshness was not checked. MULTICA_SERVER_URL and MULTICA_WORKSPACE_ID
# select the workspace.
# Env: PR_URL or MERGE_GROUP_HEAD_REF, REPO, GUARD_POLICY_JSON, GUARD_POLICY_SHA256, RUNNER_TEMP; MULTICA_BIN (default
# $RUNNER_TEMP/multica); GH_TOKEN for gh; GITHUB_EVENT_NAME (merge_group adds the REST tip line).
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

bin="${MULTICA_BIN:-$RUNNER_TEMP/multica}"
if [ -n "${GUARD_POLICY_READ_TOKEN:-}" ]; then
  if ! live="$(MULTICA_TOKEN="$GUARD_POLICY_READ_TOKEN" "$bin" workspace get --output json | jq -S -c 'if (type == "object") and (.settings | type == "object") then .settings.guard_policy // null else error("no workspace settings object") end')"; then
    echo "::error::PR guard rules: the live stored guard policy cannot be read, so the variable copy cannot be shown fresh."
    exit 2
  fi
  copy=null
  if [ "$policy_set" = 1 ] && ! copy="$(printf '%s' "$GUARD_POLICY_JSON" | jq -S -c .)"; then
    echo "::error::PR guard rules: GUARD_POLICY_JSON is not valid JSON."
    exit 2
  fi
  if [ "$live" != "$copy" ]; then
    echo "::error::PR guard rules: the variable policy copy is stale: it differs from the live stored guard policy. Update GUARD_POLICY_JSON and GUARD_POLICY_SHA256 from the stored policy."
    exit 2
  fi
elif [ "${GUARD_POLICY_LIVE_REQUIRED:-}" = 1 ]; then
  echo "::error::PR guard rules: GUARD_POLICY_LIVE_REQUIRED is set but GUARD_POLICY_READ_TOKEN is not, so the live stored policy cannot be read."
  exit 2
else
  echo "::notice::PR guard rules: no GUARD_POLICY_READ_TOKEN, so the variable policy copy is not checked against the live stored policy."
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

rc=0
"$bin" guard check-pr "$pr" -R "$REPO" || rc=$?

# CHE-8780 (CHE-7066 criterion 8): a merge-group run also prints one REST read of the live base tip, so the run shows
# the tip the CLI judged (its "guard: repo rules: ..." note) next to the tip REST reports at the same time. Print only:
# this read never changes the result, the CLI exit code stays the only decision.
if [ "${GITHUB_EVENT_NAME:-}" = merge_group ] && [[ "${MERGE_GROUP_HEAD_REF:-}" =~ gh-readonly-queue/(.+)/pr-[0-9]+-[0-9a-f]{7,40}$ ]]; then
  base="${BASH_REMATCH[1]}"
  if tip="$(timeout 30 gh api "repos/$REPO/commits/heads/$base" --jq .sha 2>&1)" && [[ "$tip" =~ ^[0-9a-f]{40}$ ]]; then
    echo "REST tip: $tip ($base of $REPO, read $(date -u +%Y-%m-%dT%H:%M:%SZ))"
  else
    echo "REST tip unavailable: $(printf '%s' "$tip" | head -n 1 | cut -c1-200)"
  fi
fi
exit "$rc"
