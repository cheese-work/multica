#!/usr/bin/env bash
set -euo pipefail

# D2 entrypoint/Compose integration test (CHE-372). Builds the actual
# repository image (same Dockerfile used for real releases), brings up
# deploy/cd/d2-controller.compose.yml with it, and proves the new CD
# entrypoint is genuinely wired in rather than merely written and tested
# in isolation — Astra's review flagged that no changed Compose file
# installed an override and no entrypoint/Compose integration receipt
# existed. This test IS that receipt.
#
# Never touches C00; every container here is local/throwaway.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

image_tag="che372-d2-entrypoint-test:$$"
project_name="che372d2ep$$"
compose_file="deploy/cd/d2-controller.compose.yml"
decision_dir="$(mktemp -d)"
candidate_port=$(( (RANDOM % 2000) + 28000 ))

# Exported for the rest of this script's lifetime so every docker compose
# invocation — up, down, ps, whatever — sees the same interpolation
# values, rather than repeating a prefix on some call sites and missing
# others (which silently uses stale/undefined values instead of failing
# loudly, since compose only rejects an interpolation it cannot resolve
# at all, not one resolved differently than the caller intended).
export D2_CANDIDATE_IMAGE="$image_tag"
export D2_CANDIDATE_PORT="$candidate_port"
export D2_ATTEMPT_ID="unset"
export D2_DECISION_WAIT_SECONDS="30"

compose() {
  docker compose -p "$project_name" -f "$compose_file" "$@"
}

# candidate_answers_http checks whether the candidate server is actually
# listening and answering HTTP, not merely whether the host port accepts a
# TCP connection. curl's %{http_code} prints the literal string "000" on
# a connection failure/reset (e.g. the port is forwarded by Docker but
# nothing is bound to it yet inside the container, which is exactly the
# gate-holding case this test needs to distinguish) — "000" is non-empty,
# so a bare `[ -n "$(curl -w '%{http_code}' ...)" ]` check is silently
# wrong and reports "answered" for a reset connection.
candidate_answers_http() {
  local code
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${candidate_port}/" 2>/dev/null)"
  [ -n "$code" ] && [ "$code" != "000" ]
}

cleanup() {
  compose down -v >/dev/null 2>&1 || true
  docker rmi "$image_tag" >/dev/null 2>&1 || true
  rm -f deploy/cd/d2-decision
  rm -rf "$decision_dir"
}
trap cleanup EXIT

pass=0
fail=0

echo "==> building the actual repository image (same Dockerfile as real releases)"
docker build -t "$image_tag" . >/dev/null

# The Compose file's candidate service mounts ./d2-decision relative to
# its own file location; symlink our throwaway per-run decision dir there
# so multiple concurrent test runs never collide on the same host path.
ln -sfn "$decision_dir" deploy/cd/d2-decision

echo "==> bringing up the Compose stack with an entrypoint override and no decision written yet"
D2_ATTEMPT_ID="test-attempt-block" D2_DECISION_WAIT_SECONDS=6 compose up -d >/dev/null

sleep 1
if candidate_answers_http; then
  echo "FAIL: candidate server answered HTTP before any migration decision was written — entrypoint gate is NOT wired in"
  fail=$((fail + 1))
else
  echo "PASS: candidate server does not answer HTTP before a migration decision is written (gate is holding)"
  pass=$((pass + 1))
fi

candidate_container="$(compose ps -aq candidate)"
if docker logs "$candidate_container" 2>&1 | grep -q "Waiting for external D2 migration decision"; then
  echo "PASS: container logs confirm the overridden entrypoint.cd.sh is what actually ran (not the stock entrypoint.sh)"
  pass=$((pass + 1))
else
  echo "FAIL: container logs do not show the CD entrypoint's wait message — the override did not take effect"
  fail=$((fail + 1))
fi

echo "==> waiting for the unwritten-decision timeout"
sleep 7
if docker logs "$candidate_container" 2>&1 | grep -q "No migration decision was recorded"; then
  echo "PASS: entrypoint correctly refused to start the server after the wait window elapsed with no decision"
  pass=$((pass + 1))
else
  echo "FAIL: entrypoint did not report the expected timeout refusal"
  fail=$((fail + 1))
fi
exited_code="$(docker inspect "$candidate_container" --format '{{.State.ExitCode}}' 2>/dev/null || echo "?")"
if [ "$exited_code" = "1" ]; then
  echo "PASS: candidate container exited 1 rather than starting the server"
  pass=$((pass + 1))
else
  echo "FAIL: expected candidate container exit code 1, got $exited_code"
  fail=$((fail + 1))
fi

echo "==> negative control: a decision file naming a DIFFERENT attempt id must be rejected as stale"
compose down -v >/dev/null 2>&1 || true
printf 'starting_candidate\nsome-other-attempt\n' > "$decision_dir/decision.txt"
D2_ATTEMPT_ID="test-attempt-fresh" D2_DECISION_WAIT_SECONDS=4 compose up -d >/dev/null
sleep 5
candidate_container="$(compose ps -aq candidate)"
if docker logs "$candidate_container" 2>&1 | grep -q "treating as stale"; then
  echo "PASS: a decision file for a different attempt id is correctly treated as stale, not admitted"
  pass=$((pass + 1))
else
  echo "FAIL: stale decision file (different attempt id) was not rejected as expected"
  docker logs "$candidate_container" 2>&1 | tail -5
  fail=$((fail + 1))
fi
if candidate_answers_http; then
  echo "FAIL: candidate server answered HTTP despite only a stale-attempt decision file — stale acceptance is possible"
  fail=$((fail + 1))
else
  echo "PASS: candidate server did not start on a stale-attempt decision file"
  pass=$((pass + 1))
fi

echo "==> positive control: a decision file naming the CURRENT attempt id is admitted"
compose down -v >/dev/null 2>&1 || true
rm -f "$decision_dir/decision.txt"
D2_ATTEMPT_ID="test-attempt-final" D2_DECISION_WAIT_SECONDS=15 compose up -d >/dev/null
sleep 2
printf 'starting_candidate\ntest-attempt-final\n' > "$decision_dir/decision.txt"
candidate_container="$(compose ps -aq candidate)"

admitted=false
for _ in $(seq 1 20); do
  if docker logs "$candidate_container" 2>&1 | grep -q "External controller admitted starting_candidate for attempt=test-attempt-final"; then
    admitted=true
    break
  fi
  sleep 0.5
done
if $admitted; then
  echo "PASS: entrypoint admitted the matching-attempt decision and proceeded to start the server"
  pass=$((pass + 1))
else
  echo "FAIL: entrypoint did not admit the matching-attempt decision"
  docker logs "$candidate_container" 2>&1 | tail -10
  fail=$((fail + 1))
fi

reachable=false
for _ in $(seq 1 20); do
  if candidate_answers_http; then
    reachable=true
    break
  fi
  sleep 0.5
done
if $reachable; then
  echo "PASS: candidate server is reachable after the matching decision was admitted"
  pass=$((pass + 1))
else
  echo "FAIL: candidate server is not reachable even after the matching decision should have admitted it"
  fail=$((fail + 1))
fi

echo
echo "==> $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
