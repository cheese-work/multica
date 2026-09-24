#!/usr/bin/env bash
# Record the required-check evidence admission.mjs verifies, once every
# required check for the commit has a final result.
#
# cd-qualification finishes long before CI's `backend` does — `backend` is a
# `needs:`-gated job, so its check run does not even exist until the jobs it
# aggregates are done. Snapshotting on cd-qualification's completion therefore
# recorded a running check as a failure (run 35822016641). This re-reads the
# check runs until `admission.mjs pending` reports nothing left to wait for,
# then keeps that snapshot. Terminal results, failures included, end the wait
# immediately and still fail admission; a check still pending at the deadline
# is recorded as-is and admission refuses it. Nothing here excuses a check.
#
# usage: record-checks.sh <source-sha> <output>
# env:   GITHUB_REPOSITORY, GH_TOKEN,
#        CHECKS_WAIT_SECONDS (default 3600), CHECKS_POLL_SECONDS (default 30)
set -euo pipefail

source_sha=$1
output=$2
wait_seconds=${CHECKS_WAIT_SECONDS:-3600}
poll_seconds=${CHECKS_POLL_SECONDS:-30}

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Some required checks are path-filtered and legitimately do not run on a
# commit that touches none of their paths (`mobile` is one). Admission must be
# able to tell "excluded" from "missing", so record the raw evidence for that
# decision rather than the decision itself: the commit's changed files, and
# each check's declared path filter read out of the workflow that defines it.
# admission.mjs re-derives the match; nothing here asserts a check may be
# skipped. The workflows are parsed with a real YAML parser (see
# workflow-path-filters.py for why), so run ensure-pyyaml.sh first.
gh api "repos/${GITHUB_REPOSITORY}/commits/${source_sha}" \
  --jq '{files: [.files[]?.filename], truncated: ((.files | length) >= 300)}' > "$work_dir/commit-files.json"
python3 deploy/cd/workflow-path-filters.py > "$work_dir/path-filters.json"

deadline=$(( $(date +%s) + wait_seconds ))
while :; do
  # admission.mjs requires backend, frontend, mobile and cd-qualification to
  # all be "success" for this exact SHA. Take them from the commit's check
  # runs — the same source a human reading the PR would use — rather than
  # asserting them here. A running check has a null conclusion.
  gh api "repos/${GITHUB_REPOSITORY}/commits/${source_sha}/check-runs?per_page=100" \
    --paginate --jq '.check_runs[] | {name, conclusion}' > "$work_dir/check-runs.jsonl"

  SOURCE_SHA="$source_sha" node -e '
    const fs = require("node:fs");
    const [dir, output] = process.argv.slice(1);

    const contexts = {};
    for (const line of fs.readFileSync(`${dir}/check-runs.jsonl`, "utf8").split("\n")) {
      if (!line.trim()) continue;
      const run = JSON.parse(line);
      contexts[run.name] = run.conclusion;
    }

    const commit = JSON.parse(fs.readFileSync(`${dir}/commit-files.json`, "utf8"));
    const pathFilters = JSON.parse(fs.readFileSync(`${dir}/path-filters.json`, "utf8"));

    fs.writeFileSync(output, `${JSON.stringify({
      sha: process.env.SOURCE_SHA,
      contexts,
      changed_files: commit.files ?? [],
      changed_files_truncated: Boolean(commit.truncated),
      path_filters: pathFilters,
    }, null, 2)}\n`);
  ' "$work_dir" "$output"

  pending="$(node deploy/cd/admission.mjs pending --checks "$output")"
  if [ -z "$pending" ]; then
    echo "required checks settled for ${source_sha}"
    exit 0
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "::warning::required checks still pending after ${wait_seconds}s: $(echo $pending); recording them as pending"
    exit 0
  fi
  echo "waiting for required checks: $(echo $pending)"
  sleep "$poll_seconds"
done
