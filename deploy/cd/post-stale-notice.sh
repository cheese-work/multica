#!/usr/bin/env bash
# CHE-5352: post the stale-candidate line (NOTICE, from resolve-candidate) as one
# comment on MULTICA_NOTICE_ISSUE_ID. Runs on X99, which can reach the tailnet-only
# Multica server. Best effort: no notice, no token or a failed post never fails the job.
set -euo pipefail

[ -n "${NOTICE:-}" ] || exit 0
if [ -z "${MULTICA_SERVER_URL:-}" ] || [ -z "${MULTICA_TOKEN:-}" ] || [ -z "${MULTICA_NOTICE_ISSUE_ID:-}" ]; then
  echo "::warning::stale-candidate notice not posted: Multica server, token or issue not configured"
  exit 0
fi
headers="$(mktemp)"
trap 'rm -f "$headers"' EXIT
# The token goes through a header file, not argv.
printf 'Authorization: Bearer %s\nX-Workspace-ID: %s\n' "$MULTICA_TOKEN" "${MULTICA_WORKSPACE_ID:-}" >"$headers"
python3 -c 'import json,sys; print(json.dumps({"content": sys.argv[1]}))' "$NOTICE" \
  | curl -fsS --max-time 20 -H 'Content-Type: application/json' -H @"$headers" \
      -d @- "${MULTICA_SERVER_URL%/}/api/issues/$MULTICA_NOTICE_ISSUE_ID/comments" >/dev/null \
  || echo "::warning::could not post the stale-candidate notice to the notice issue"
