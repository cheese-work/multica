#!/usr/bin/env bash
set -euo pipefail

# CHE-2418 B5: tell the approver a C00 deploy is waiting. POSTs the version and
# the run link, where GitHub's "Review deployments" button lives, to
# WEBHOOK_URL (ntfy-style: the JSON body carries `message` and `click`).
# Best effort: the approval gate does not depend on this notice, so no webhook
# or a failed POST only warns.

: "${SOURCE_SHA:?}" "${RUN_URL:?}"

if [ -z "${WEBHOOK_URL:-}" ]; then
  echo "::notice title=C00 approval notice::C00_APPROVAL_WEBHOOK_URL is not set; no notice sent. Approve at $RUN_URL"
  exit 0
fi

version="${SOURCE_SHA:0:12}"
body="$(python3 -c '
import json, sys
version, url = sys.argv[1:3]
print(json.dumps({"title": "C00 deploy waiting for approval",
                  "message": f"Version {version} is ready. Open the run and press Review deployments.",
                  "click": url}))' "$version" "$RUN_URL")"

# The body goes over stdin so the webhook URL's secret path stays out of the
# logged command and the message stays out of argv.
if ! printf '%s' "$body" | curl -fsS --max-time 20 -H 'Content-Type: application/json' -d @- "$WEBHOOK_URL" >/dev/null; then
  echo "::warning title=C00 approval notice::webhook POST failed; approve at $RUN_URL"
fi
