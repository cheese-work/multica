#!/bin/sh
set -eu

# D2 CD deployment-mode entrypoint (CHE-372). This is NOT the default
# container entrypoint (see entrypoint.sh, unchanged for non-CD use). It
# exists because the stock entrypoint runs `./migrate up` then immediately
# `exec ./server` with no external gate — exactly the auto-chain the
# approved D2 design says must not be trusted for the CD path.
#
# Use this entrypoint only via an explicit, reviewed Compose command/
# entrypoint override for the D2 controller's own candidate container
# (see deploy/cd/d2-controller.compose.yml), never as a general-purpose
# replacement for entrypoint.sh. It changes release proof for whatever
# image uses it, per the addendum's note that entrypoint changes are real
# D2 code.
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
# writes exactly two lines to:
#   starting_candidate
#   <attempt id>
# CHE372_D2_ATTEMPT_ID must match the second line exactly for the decision
# to be accepted — this is the freshness/stale-acceptance check Astra's
# review named: a decision file left over from an earlier attempt (a
# previous container, a previous cutover try) must never be readable as
# current just because a file with the right first line happens to exist
# at that path. The attempt id is caller-supplied (e.g. a UUID or the
# cutover's own timestamp+source-SHA) precisely so a fresh attempt cannot
# accidentally collide with a stale one.
#
# Any other content on the first line, a missing attempt-id line, an
# attempt-id mismatch, or the file being absent/unreadable are all treated
# as "not yet decided" and this script keeps waiting up to
# CHE372_D2_DECISION_WAIT_SECONDS, then exits non-zero rather than
# starting the server on an unproven or stale migration.

decision_file="${CHE372_D2_MIGRATION_DECISION_FILE:?CHE372_D2_MIGRATION_DECISION_FILE is required for the D2 CD entrypoint}"
attempt_id="${CHE372_D2_ATTEMPT_ID:?CHE372_D2_ATTEMPT_ID is required for the D2 CD entrypoint}"
wait_seconds="${CHE372_D2_DECISION_WAIT_SECONDS:-30}"

echo "Waiting for external D2 migration decision at $decision_file for attempt=$attempt_id (up to ${wait_seconds}s)..."

elapsed=0
while [ "$elapsed" -lt "$wait_seconds" ]; do
  if [ -f "$decision_file" ]; then
    decision="$(sed -n '1p' "$decision_file" 2>/dev/null || true)"
    decision_attempt_id="$(sed -n '2p' "$decision_file" 2>/dev/null || true)"
    if [ "$decision" = "starting_candidate" ] && [ -n "$decision_attempt_id" ] && [ "$decision_attempt_id" = "$attempt_id" ]; then
      echo "External controller admitted starting_candidate for attempt=$attempt_id. Starting server without replaying migrations."
      exec ./server
    fi
    if [ "$decision" = "starting_candidate" ]; then
      echo "Decision file present with starting_candidate but attempt id mismatch (file has '$decision_attempt_id', expected '$attempt_id') — treating as stale, refusing to start server." >&2
    else
      echo "Decision file present but not admitted (contents: '$decision'); refusing to start server." >&2
    fi
    exit 1
  fi
  sleep 1
  elapsed=$((elapsed + 1))
done

echo "No migration decision was recorded within ${wait_seconds}s; refusing to start server." >&2
exit 1
