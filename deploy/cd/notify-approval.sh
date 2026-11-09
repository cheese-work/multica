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
  local headers api list payload ok=0
  headers="$(mktemp)"
  # The token goes through a header file, not argv.
  printf 'Authorization: Bearer %s\nX-Workspace-ID: %s\n' "$MULTICA_TOKEN" "$MULTICA_WORKSPACE_ID" >"$headers"
  api="${MULTICA_SERVER_URL%/}/api/issues/$MULTICA_NOTIFY_ISSUE/comments"
  list="$(mktemp)"
  if curl_json -H "@$headers" "$api?roots_only=true" >"$list" \
     && payload="$(python3 -c '
import json, sys
list_file, sha, version, url, member, repo = sys.argv[1:7]
marker = "<!-- cd-deploy-candidate -->"
asks = [c for c in json.load(open(list_file))
        if marker in (c.get("content") or "") and not c.get("resolved_at") and not c.get("superseded_by_id")]
if any(sha in c["content"] for c in asks):
    sys.exit(0)  # this SHA is already asked
cmd = f"gh workflow run cd-deploy.yml --repo {repo} --ref main -f candidate_sha={sha}"
content = (f"[@Cheese](mention://member/{member}) C00 candidate `{version}` is qualified and waiting for your deploy.\n\n"
           f"Dispatch it:\n\n```bash\n{cmd}\n```\n\nRun: {url}\n\n{marker}")
print(json.dumps({"content": content, "supersedes": [c["id"] for c in asks]}))' \
        "$list" "$SOURCE_SHA" "$version" "$RUN_URL" "$MULTICA_NOTIFY_MEMBER_ID" "${GITHUB_REPOSITORY:-cheese-work/multica}")"; then
    ok=1
    if [ -n "$payload" ]; then
      printf '%s' "$payload" | curl_json -H "@$headers" -d @- "$api" >/dev/null || ok=0
    fi
  fi
  rm -f "$headers" "$list"
  [ "$ok" = 1 ]
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
