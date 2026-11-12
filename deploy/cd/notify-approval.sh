#!/usr/bin/env bash
set -euo pipefail

# CHE-2418 B5 / CHE-4879: tell the owner a qualified candidate is ready. Two
# routes, either or both:
#   - WEBHOOK_URL (ntfy-style: the JSON body carries `message` and `click`);
#   - Multica: one decision card for MULTICA_NOTIFY_MEMBER_ID on
#     MULTICA_NOTIFY_ISSUE, holding the ready-to-run dispatch one-liner
#     (POST /api/decisions/ci, scope decisions:ci_request). The server keeps one
#     open card per SHA, withdraws an older one and refuses a stale SHA.
#     Needs MULTICA_SERVER_URL, MULTICA_TOKEN, MULTICA_WORKSPACE_ID and
#     MULTICA_NOTIFY_SOURCE_COMMENT (the anchor comment) too.
# No route configured, or no route delivered, fails the job: a silent notice
# left eligible candidates unseen for a day. The deploy gate itself does not
# depend on this job.
#
# SOURCE_SHA comes from the schedule-resolve job. A manual dispatch has none, so
# INPUT_REF (a branch, tag or commit SHA) is fetched and peeled to its commit:
# an annotated tag's FETCH_HEAD is the tag object, not the version to deploy.

: "${RUN_URL:?}"

if [ -z "${SOURCE_SHA:-}" ]; then
  if ! { git fetch --quiet --depth=1 origin "${INPUT_REF:?SOURCE_SHA or INPUT_REF}" \
         && SOURCE_SHA="$(git rev-parse --verify "FETCH_HEAD^{commit}")"; }; then
    echo "::warning title=C00 approval notice::cannot resolve ref '$INPUT_REF' to a commit; no notice sent. Dispatch at $RUN_URL"
    exit 0
  fi
fi

version="${SOURCE_SHA:0:12}"
curl_json() { curl -fsS --max-time 20 -H 'Content-Type: application/json' "$@"; }

notify_webhook() {
  local body
  body="$(python3 -c '
import json, sys
version, full, url = sys.argv[1:4]
print(json.dumps({"title": "C00 candidate ready to deploy",
                  "message": f"Version {version} is qualified. Run cd-deploy with candidate_sha={full}.",
                  "click": url}))' "$version" "$SOURCE_SHA" "$RUN_URL")"
  # The body goes over stdin so the webhook URL's secret path stays out of the
  # logged command and the message stays out of argv.
  printf '%s' "$body" | curl_json -d @- "$WEBHOOK_URL" >/dev/null
}

notify_multica() {
  local headers resp src at payload code api
  headers="$(mktemp)" resp="$(mktemp)"
  trap 'rm -f "$headers" "$resp"' RETURN
  # The token goes through a header file, not argv.
  printf 'Authorization: Bearer %s\nX-Workspace-ID: %s\n' "$MULTICA_TOKEN" "$MULTICA_WORKSPACE_ID" >"$headers"
  api="${MULTICA_SERVER_URL%/}/api"
  # The card anchors on MULTICA_NOTIFY_SOURCE_COMMENT (ask.source_comment_id), a root
  # comment of the notice issue. A scoped PAT may call only POST /api/decisions/ci,
  # so the script never lists comments.
  src="$MULTICA_NOTIFY_SOURCE_COMMENT"
  # The server orders cards by the candidate's commit time.
  at="$(gh api "repos/${GITHUB_REPOSITORY:-cheese-work/multica}/commits/$SOURCE_SHA" -q .commit.committer.date)" || return 1
  payload="$(python3 -c '
import json, sys
sha, version, url, issue, member, repo, src, at = sys.argv[1:]
cmd = f"gh workflow run cd-deploy.yml --repo {repo} --ref main -f candidate_sha={sha}"
def option(label, what, pro, con, cost):
    return {"label": label, "what_happens": what, "pros": [pro], "cons": [con], "cost_or_time": cost}
ask = {
    "context_eli10": f"Candidate {version} passed every check and is waiting. C00 runs older code until you dispatch it.",
    "agent_did": f"The CD preflight marked {version} eligible. Run: {url}",
    "decision": f"Deploy candidate {version} to C00?",
    "stakes_if_wrong": "A held candidate leaves C00 on older code; a bad deploy is rolled back by digest.",
    "options": [
        option("Deploy", f"Run this on a machine with gh access: `{cmd}`", "C00 gets the qualified candidate", "Replaces the running version", "About 15 minutes"),
        option("Hold", "Nothing is deployed; a newer eligible candidate replaces this ask.", "No change on C00", "C00 stays on older code", "None"),
    ],
    "recommendation": "Deploy: the candidate passed preflight.",
    "recommended_option": "Deploy",
    "default_if_no_reply": "Hold; C00 stays as it is.",
    "source_issue_id": issue,
    "source_comment_id": src,
}
print(json.dumps({"issue_id": issue, "for": member, "ask": ask, "candidate_sha": sha, "candidate_at": at}))' \
    "$SOURCE_SHA" "$version" "${RUN_LINK:-$RUN_URL}" "$MULTICA_NOTIFY_ISSUE" "$MULTICA_NOTIFY_MEMBER_ID" "${GITHUB_REPOSITORY:-cheese-work/multica}" "$src" "$at")" || return 1
  code="$(printf '%s' "$payload" | curl -sS --max-time 20 -H 'Content-Type: application/json' -H "@$headers" \
    -o "$resp" -w '%{http_code}' -d @- "$api/decisions/ci")" || return 1
  # 201: new card. 200: this SHA already has one. 409: a newer candidate does.
  case "$code" in
    200|201|409) return 0 ;;
    *) echo "::warning title=C00 approval notice::decision card refused ($code)"; return 1 ;;
  esac
}

routes=0 delivered=0
if [ -n "${WEBHOOK_URL:-}" ]; then
  routes=$((routes + 1))
  if notify_webhook; then delivered=$((delivered + 1)); else echo "::warning title=C00 approval notice::webhook POST failed"; fi
fi
if [ -n "${MULTICA_TOKEN:-}" ] && [ -n "${MULTICA_SERVER_URL:-}" ] && [ -n "${MULTICA_WORKSPACE_ID:-}" ] \
   && [ -n "${MULTICA_NOTIFY_ISSUE:-}" ] && [ -n "${MULTICA_NOTIFY_MEMBER_ID:-}" ] \
   && [ -n "${MULTICA_NOTIFY_SOURCE_COMMENT:-}" ]; then
  routes=$((routes + 1))
  if notify_multica; then delivered=$((delivered + 1)); else echo "::warning title=C00 approval notice::Multica decision card failed"; fi
fi

if [ "$routes" -eq 0 ]; then
  echo "::error title=C00 approval notice::no notice route configured (set C00_APPROVAL_WEBHOOK_URL or the MULTICA_* secrets); candidate $version is waiting. Dispatch at $RUN_URL"
  exit 1
fi
if [ "$delivered" -eq 0 ]; then
  echo "::error title=C00 approval notice::every notice route failed; candidate $version is waiting. Dispatch at $RUN_URL"
  exit 1
fi
