#!/usr/bin/env bash
# CHE-4998: resolve which commit a cd-deploy dispatch deploys; writes `sha=` to
# $GITHUB_OUTPUT. A typed CANDIDATE_SHA wins. With no SHA and no evidence path,
# pick the newest main commit whose "CD qualification build" push run succeeded
# (`gh run list` is newest first). Any evidence path set -> `sha=` stays empty
# and the manual-evidence flow runs unchanged. Admission, freshness and
# quiescence still gate the result downstream.
# CHE-5352: when the qualified candidate is behind main, say so in the job
# summary; more than 20 commits or 2 h behind also gets a ::warning and a
# `notice=` output that the X99 job runs through post-stale-notice.sh (CHE-4960
# line). This never changes which commit deploys or fails the job.
set -euo pipefail

notice_if_behind() {
  local repo="${GITHUB_REPOSITORY:?}" sha="$1" main main_sha main_at cand_at behind run run_url when msg
  main="$(gh api "repos/$repo/commits/main" --jq '[.sha, .commit.committer.date] | @tsv')" || return 0
  main_sha="${main%%$'\t'*}" main_at="${main##*$'\t'}"
  [ -n "$main_sha" ] && [ "$main_sha" != "$sha" ] || return 0
  behind="$(gh api "repos/$repo/compare/$sha...main" --jq .ahead_by)" || return 0
  cand_at="$(gh api "repos/$repo/commits/$sha" --jq .commit.committer.date)" || return 0
  [[ "$behind" =~ ^[0-9]+$ ]] && [ "$behind" -gt 0 ] && [ -n "$cand_at" ] || return 0
  when="$(TZ=Asia/Ho_Chi_Minh date -d "$cand_at" '+%-d %b %Y %H:%M ICT')" || return 0
  # Newest main qualification run of any status: the failed or missing one.
  run="$(gh run list --repo "$repo" --workflow "CD qualification build" --branch main --event push \
    --limit 1 --json headSha,url --jq '.[0] | select(. != null) | "\(.headSha) \(.url)"')" || run=""
  run_url="${run#* }"
  [ "${run%% *}" = "$main_sha" ] && [ -n "$run_url" ] || run_url="https://github.com/$repo/actions/workflows/cd-qualification.yml"
  msg="Deploying $sha ($when), $behind commits behind main. Newest main commit failed or has not run qualification: $run_url."
  echo "$msg" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
  local age=$(( $(date -d "$main_at" +%s) - $(date -d "$cand_at" +%s) ))
  if [ "$behind" -gt 20 ] || [ "$age" -gt 7200 ]; then
    echo "::warning title=Stale CD candidate::$msg"
    # Hosted runner: it cannot reach the tailnet Multica server, so the text goes
    # out as a job output for the X99 post job (post-stale-notice.sh).
    echo "notice=$msg" >>"$GITHUB_OUTPUT"
  fi
}

sha="${CANDIDATE_SHA:-}"
if [ -z "$sha" ] && [ -z "${INPUT_MANIFEST:-}${INPUT_BASELINE:-}${INPUT_EVENT:-}${INPUT_CHECKS:-}${INPUT_PROVENANCE:-}" ]; then
  sha="$(gh run list --repo "$GITHUB_REPOSITORY" --workflow "CD qualification build" \
    --branch main --event push --status success --limit 1 \
    --json headSha --jq '.[0].headSha // empty')"
  [ -n "$sha" ] || { echo "::error::no successful main qualification run to deploy; pass candidate_sha or wait for a qualified main commit" >&2; exit 1; }
  echo "Empty dispatch: deploying the newest qualified main commit $sha" >&2
  echo "Empty dispatch resolved to the newest qualified main commit: \`$sha\`" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
  notice_if_behind "$sha" || true
fi
echo "sha=$sha" >>"$GITHUB_OUTPUT"
