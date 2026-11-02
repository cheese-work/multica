#!/usr/bin/env bash
set -euo pipefail

# CHE-2418 B5: tell the approver a C00 deploy is waiting. POSTs the version and
# the run link, where GitHub's "Review deployments" button lives, to
# WEBHOOK_URL (ntfy-style: the JSON body carries `message` and `click`).
# Best effort: the approval gate does not depend on this notice, so no webhook
# or a failed POST only warns.
#
# SOURCE_SHA comes from the workflow_run payload. A manual dispatch has none, so
# INPUT_REF (a branch, tag or commit SHA) is fetched and peeled to its commit:
# an annotated tag's FETCH_HEAD is the tag object, not the version to deploy.

: "${RUN_URL:?}"

if [ -z "${SOURCE_SHA:-}" ]; then
  if ! { git fetch --quiet --depth=1 origin "${INPUT_REF:?SOURCE_SHA or INPUT_REF}" \
         && SOURCE_SHA="$(git rev-parse --verify "FETCH_HEAD^{commit}")"; }; then
    echo "::warning title=C00 approval notice::cannot resolve ref '$INPUT_REF' to a commit; no notice sent. Approve at $RUN_URL"
    exit 0
  fi
fi

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
