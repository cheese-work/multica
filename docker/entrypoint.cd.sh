#!/bin/sh
set -eu

# D2 CD deployment-mode entrypoint (CHE-372). This is NOT the default
# container entrypoint (see entrypoint.sh, unchanged for non-CD use). It
# exists because the stock entrypoint runs `./migrate up` then immediately
# `exec ./server` with no external gate — exactly the auto-chain the
# approved D2 design says must not be trusted for the CD path.
#
# Use this entrypoint only via an explicit, reviewed Compose command/
# entrypoint override for the D2 controller's own candidate container,
# never as a general-purpose replacement for entrypoint.sh. It changes
# release proof for whatever image uses it, per the addendum's note that
# entrypoint changes are real D2 code.
#
# Contract: this script does NOT decide quiescence or run the supervised
# migration itself — the external controller does that (deploy/cd/
# migrate-supervised.sh, gated by deploy/cd/quiescence.mjs), and only
# signals THIS container to start the server via the sentinel file below
# once that decision is positive. This keeps the "start the app only after
# the controller's success decision" requirement enforceable from outside
# the container, where the watchdog also lives, rather than trusting the
# entrypoint's own internal migration attempt.
#
# CHE372_D2_MIGRATION_DECISION_FILE: path (inside this container, expected
# to be a bind mount shared with the controller) that the controller
# writes exactly one line to: "starting_candidate" for success, anything
# else (or file absent/unreadable) is treated as not-yet-decided and this
# script keeps waiting up to CHE372_D2_DECISION_WAIT_SECONDS, then exits
# non-zero rather than starting the server on an unproven migration.

decision_file="${CHE372_D2_MIGRATION_DECISION_FILE:?CHE372_D2_MIGRATION_DECISION_FILE is required for the D2 CD entrypoint}"
wait_seconds="${CHE372_D2_DECISION_WAIT_SECONDS:-30}"

echo "Waiting for external D2 migration decision at $decision_file (up to ${wait_seconds}s)..."

elapsed=0
while [ "$elapsed" -lt "$wait_seconds" ]; do
  if [ -f "$decision_file" ]; then
    decision="$(cat "$decision_file" 2>/dev/null || true)"
    if [ "$decision" = "starting_candidate" ]; then
      echo "External controller admitted starting_candidate. Starting server without replaying migrations."
      exec ./server
    fi
    echo "Decision file present but not admitted (contents: '$decision'); refusing to start server."
    exit 1
  fi
  sleep 1
  elapsed=$((elapsed + 1))
done

echo "No migration decision was recorded within ${wait_seconds}s; refusing to start server." >&2
exit 1
