#!/usr/bin/env bash
# CHE-9542: interim merge freeze until PR 1834 (upstream v0.10.0 sync, CHE-6568) merges. While the repository variable
# SYNC_FREEZE is `on` (any case), this check fails every PR except PR 1834 and a PR labelled `urgent`. Any other value
# or unset: no freeze, nothing is read. The permanent rule is CHE-9549, which reuses this check.
# Runs inside the reusable PR guard rules job, so it covers pull_request_target, the edit workflow and every merge-queue
# build with the same required context. A merge-queue build has no PR payload: the PR number comes from the queue branch
# name (refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha>), as in pr-guard-rules.sh. Labels are read live through the
# read-only workflow token, so a label added after the PR event is judged when the job is re-run and again at queue time (no label trigger: CHE-4385 run budget).
# Turn the freeze on with scripts/sync-freeze-activate.sh (drains the merge queue first; entries already checked keep their old result).
# Fail closed: while frozen, an unreadable PR number or label list is red. Lift: `gh variable set SYNC_FREEZE --body off`.
# Env: SYNC_FREEZE, PR_URL or MERGE_GROUP_HEAD_REF, REPO, GH_TOKEN; SYNC_FREEZE_ALLOWED_PR (default 1834).
set -euo pipefail

freeze="$(printf '%s' "${SYNC_FREEZE:-}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')"
if [ "$freeze" != on ]; then
  exit 0
fi

allowed="${SYNC_FREEZE_ALLOWED_PR:-1834}"
fail() {
  echo "::error::Sync freeze: $1"
  exit 1
}

n=""
if [[ "${PR_URL:-}" =~ /pull/([0-9]+)$ ]]; then
  n="${BASH_REMATCH[1]}"
elif [[ "${MERGE_GROUP_HEAD_REF:-}" =~ /pr-([0-9]+)-[0-9a-f]{7,40}$ ]]; then
  n="${BASH_REMATCH[1]}"
fi
[ -n "$n" ] || fail "merges to main are frozen and the PR number is unreadable, so the PR cannot be judged."

if [ "$n" = "$allowed" ]; then
  echo "::notice::Sync freeze: PR $n is the sync PR; allowed."
  exit 0
fi

if ! labels="$(gh api "repos/$REPO/issues/$n/labels" --paginate --jq '.[].name' 2>&1)"; then
  fail "merges to main are frozen and the labels of PR $n cannot be read ($(printf '%s' "$labels" | head -n 1 | cut -c1-200))."
fi
if printf '%s\n' "$labels" | grep -qxF urgent; then
  echo "::notice::Sync freeze: PR $n carries the urgent label; allowed."
  exit 0
fi
fail "merges to main are frozen until PR $allowed merges (CHE-6568). Keep the PR as is; it needs no change. An urgent fix takes the \`urgent\` label."
