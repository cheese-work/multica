#!/usr/bin/env bash
# CHE-4251: choose the token for a CI read path. Use the c00-multicado App token while its hourly budget stays above
# the floor the Multica server needs; otherwise fall back to GITHUB_TOKEN. Never prints a token.
# env: APP_TOKEN (may be empty), FALLBACK_TOKEN, APP_TOKEN_FLOOR (default 1500), GITHUB_OUTPUT
# out: token=<chosen token> in GITHUB_OUTPUT
set -euo pipefail

floor=${APP_TOKEN_FLOOR:-1500}
remaining=""
if [ -n "${APP_TOKEN:-}" ]; then
  # /rate_limit does not count against the limit it reports.
  remaining="$(GH_TOKEN=$APP_TOKEN gh api rate_limit --jq .resources.core.remaining 2>/dev/null)" || remaining=""
fi

if [[ $remaining =~ ^[0-9]+$ ]] && [ "$remaining" -ge "$floor" ]; then
  echo "App token budget: $remaining requests left (floor $floor); using the App token"
  token=$APP_TOKEN
else
  echo "::warning::App token budget ${remaining:-unreadable or no App token} is below the floor $floor; falling back to GITHUB_TOKEN"
  token=${FALLBACK_TOKEN:?FALLBACK_TOKEN is required}
fi
echo "::add-mask::$token"
echo "token=$token" >> "$GITHUB_OUTPUT"
