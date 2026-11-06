#!/usr/bin/env bash
# CHE-4251: choose the token for a CI read path. Use the c00-multicado App token only while its hourly budget stays
# at or above floor + headroom; otherwise fall back to GITHUB_TOKEN. The floor is what the Multica server keeps; the
# headroom covers what one poll or a sibling job spends before the next read. Never prints a token.
# env: APP_TOKEN (may be empty), FALLBACK_TOKEN, APP_TOKEN_FLOOR (default 1500), APP_TOKEN_HEADROOM (default 100),
#      GITHUB_OUTPUT
# out: token=<chosen token> in GITHUB_OUTPUT
set -euo pipefail

floor=${APP_TOKEN_FLOOR:-1500}
needed=$(( floor + ${APP_TOKEN_HEADROOM:-100} ))
remaining=""
if [ -n "${APP_TOKEN:-}" ]; then
  # /rate_limit does not count against the limit it reports.
  remaining="$(GH_TOKEN=$APP_TOKEN gh api rate_limit --jq .resources.core.remaining 2>/dev/null)" || remaining=""
fi

if [[ $remaining =~ ^[0-9]+$ ]] && [ "$remaining" -ge "$needed" ]; then
  echo "App token budget: $remaining requests left (needs $needed: floor $floor plus headroom); using the App token"
  token=$APP_TOKEN
else
  echo "::warning::App token budget ${remaining:-unreadable or no App token} is below $needed (floor $floor plus headroom); falling back to GITHUB_TOKEN"
  token=${FALLBACK_TOKEN:?FALLBACK_TOKEN is required}
fi
echo "::add-mask::$token"
echo "token=$token" >> "$GITHUB_OUTPUT"
