#!/usr/bin/env bash
set -euo pipefail

# CHE-2418 B5 / CHE-4879: tell the owner a qualified candidate is ready. Two
# routes, either or both:
#   - WEBHOOK_URL (ntfy-style: the JSON body carries `message` and `click`);
#   - Multica: one comment on MULTICA_NOTIFY_ISSUE that mentions
#     MULTICA_NOTIFY_MEMBER_ID with the ready-to-run dispatch one-liner. It
#     supersedes the older open ask, and skips a SHA that is already asked.
#     Needs MULTICA_SERVER_URL, MULTICA_TOKEN and MULTICA_WORKSPACE_ID too.
# No route configured, or no route delivered, fails the job: a silent notice
# left eligible candidates unseen for a day. The deploy gate itself does not
# depend on this job.
#
# SOURCE_SHA comes from the workflow_run payload. A manual dispatch has none, so
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
  local headers api base list me asks id ask_sha status payload
  local -a supersede=()
  headers="$(mktemp)" list="$(mktemp)"
  trap 'rm -f "$headers" "$list"' RETURN
  # The token goes through a header file, not argv.
  printf 'Authorization: Bearer %s\nX-Workspace-ID: %s\n' "$MULTICA_TOKEN" "$MULTICA_WORKSPACE_ID" >"$headers"
  base="${MULTICA_SERVER_URL%/}/api"
  api="$base/issues/$MULTICA_NOTIFY_ISSUE/comments"
  # Only asks written by this token's own user count: anyone else can paste the
  # marker, which must neither hide a candidate nor get a real ask superseded.
  me="$(curl_json -H "@$headers" "$base/me" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')" || return 1
  curl_json -H "@$headers" "$api?roots_only=true" >"$list" || return 1
  asks="$(python3 -c '
import json, sys
list_file, me = sys.argv[1:3]
marker = "<!-- cd-deploy-candidate -->"
for c in json.load(open(list_file)):
    text = c.get("content") or ""
    if marker in text and c.get("author_id") == me and not c.get("resolved_at") and not c.get("superseded_by_id"):
        sha = text.split("candidate_sha=")[-1].split()[0].strip("`")
        print(c["id"], sha)' "$list" "$me")" || return 1
  # This SHA is already asked: nothing to send.
  while read -r id ask_sha; do
    [ "$ask_sha" != "$SOURCE_SHA" ] || return 0
  done <<<"$asks"
  # Freshness is decided at the write: an older candidate that finishes late must
  # not replace the ask for a newer one. A lookup failure or a diverged SHA fails.
  while read -r id ask_sha; do
    [ -n "$id" ] || continue
    status="$(gh api "repos/${GITHUB_REPOSITORY:-cheese-work/multica}/compare/$ask_sha...$SOURCE_SHA" -q .status)" || return 1
    case "$status" in
      ahead) supersede+=("$id") ;;
      behind|identical) return 0 ;; # a newer (or the same) candidate is already asked
      *) echo "::warning title=C00 approval notice::cannot order $ask_sha against $SOURCE_SHA ($status)"; return 1 ;;
    esac
  done <<<"$asks"
  payload="$(python3 -c '
import json, sys
sha, version, url, member, repo, *ids = sys.argv[1:]
cmd = f"gh workflow run cd-deploy.yml --repo {repo} --ref main -f candidate_sha={sha}"
content = (f"[@Cheese](mention://member/{member}) C00 candidate `{version}` is qualified and waiting for your deploy.\n\n"
           f"Dispatch it:\n\n```bash\n{cmd}\n```\n\nRun: {url}\n\n<!-- cd-deploy-candidate -->")
print(json.dumps({"content": content, "supersedes": ids}))' \
    "$SOURCE_SHA" "$version" "${RUN_LINK:-$RUN_URL}" "$MULTICA_NOTIFY_MEMBER_ID" "${GITHUB_REPOSITORY:-cheese-work/multica}" "${supersede[@]}")" || return 1
  printf '%s' "$payload" | curl_json -H "@$headers" -d @- "$api" >/dev/null
}

routes=0 delivered=0
if [ -n "${WEBHOOK_URL:-}" ]; then
  routes=$((routes + 1))
  if notify_webhook; then delivered=$((delivered + 1)); else echo "::warning title=C00 approval notice::webhook POST failed"; fi
fi
if [ -n "${MULTICA_TOKEN:-}" ] && [ -n "${MULTICA_SERVER_URL:-}" ] && [ -n "${MULTICA_WORKSPACE_ID:-}" ] \
   && [ -n "${MULTICA_NOTIFY_ISSUE:-}" ] && [ -n "${MULTICA_NOTIFY_MEMBER_ID:-}" ]; then
  routes=$((routes + 1))
  if notify_multica; then delivered=$((delivered + 1)); else echo "::warning title=C00 approval notice::Multica comment failed"; fi
fi

if [ "$routes" -eq 0 ]; then
  echo "::error title=C00 approval notice::no notice route configured (set C00_APPROVAL_WEBHOOK_URL or the MULTICA_* secrets); candidate $version is waiting. Dispatch at $RUN_URL"
  exit 1
fi
if [ "$delivered" -eq 0 ]; then
  echo "::error title=C00 approval notice::every notice route failed; candidate $version is waiting. Dispatch at $RUN_URL"
  exit 1
fi
