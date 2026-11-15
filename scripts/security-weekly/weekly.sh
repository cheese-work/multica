#!/usr/bin/env bash
# CHE-3701: Monday-brief step on X99. Unseal a weekly run, summarize against last week, plan and (unless --no-apply) file.
# Usage (inside a cheese-work/multica checkout): weekly.sh <run-id> <assignee-id> <project-id> [--no-apply]
# State (previous summary, work dirs) lives in $SECURITY_WEEKLY_STATE (default /mnt/ssd2/security-weekly), never in the repo.
# Runs only from the trusted install (install-trusted.sh): the secret key is in reach, so no checkout-controlled script may run here.
set -euo pipefail
run="$1"; assignee="$2"; project="$3"; apply="${4:-}"
here="$(cd "$(dirname "$0")" && pwd)"
state="${SECURITY_WEEKLY_STATE:-/mnt/ssd2/security-weekly}"
case "$here" in "$state"/trusted/*) ;; *) echo "weekly: $here is not the trusted install; run $state/trusted/current/weekly.sh (see install-trusted.sh)" >&2; exit 1 ;; esac
work="$state/runs/$run"; rm -rf "$work"; mkdir -p "$work"
bash "$here/unseal.sh" "$run" "$work/reports"
sha="$(gh run view "$run" -R cheese-work/multica --json headSha --jq .headSha)"
git fetch --quiet origin "$sha"; git worktree add --quiet --detach "$work/src" "$sha"
git remote get-url upstream >/dev/null 2>&1 || git remote add upstream https://github.com/multica-ai/multica.git
git fetch --quiet --filter=blob:none upstream main:refs/remotes/upstream/main || echo "upstream fetch failed: sharing reads unknown" >&2
prev=-; [ -f "$state/summary.json" ] && prev="$state/summary.json"
rc=0; node "$here/summarize.mjs" "$work/reports" "$prev" "$work/summary.json" "$work/src" | tee "$work/lines.txt" || rc=$?
node "$here/triage.mjs" "$work/summary.json" "$work/triage.json"
if [ "$apply" = "--no-apply" ]; then echo "plan only: $work/triage.json"; exit "$rc"; fi
node "$here/triage.mjs" --apply "$work/triage.json" "$assignee" "$project"
cp "$work/summary.json" "$state/summary.json" # advance the baseline only after every issue was filed
exit "$rc"
