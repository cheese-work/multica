#!/usr/bin/env bash
set -euo pipefail

# Behavioral control-flow test for cutover.sh (CHE-397 unit 2): the A/B
# slot forward-cutover and rollback sequencing built on top of deploy-lib.sh
# and delegating to quiescence.mjs/release-packet.mjs/router.sh for their
# own gates. Mirrors test-deploy.sh's approach — docker/curl/node are
# scripted mocks driven by control files a scenario writes before invoking
# cutover.sh, so no real Docker daemon, Postgres, or Nginx is required.
#
# node is mocked selectively: quiescence.mjs calls are intercepted and
# answer from a control file (admit/deny), everything else (release-packet.mjs,
# router.sh's node calls, json_field) delegates to the REAL node binary,
# since those are exercised by their own dedicated test scripts and this
# test only needs to prove cutover.sh's own sequencing/gating logic.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

real_node="$(command -v node)"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

mock_bin="$work_dir/bin"
mkdir -p "$mock_bin"

source_sha="504078f8ea7fa31f342f195659e93a7f6c3e5a91"
backend_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
web_digest="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
# Distinct from backend_digest/web_digest on purpose: these stand in for
# whatever the retained predecessor colour is ACTUALLY running, which a
# correct rollback must resolve independently of the candidate's own
# manifest-pinned digests above (CHE-678).
incumbent_backend_digest="sha256:$(printf 'c%.0s' {1..64})"
incumbent_web_digest="sha256:$(printf 'd%.0s' {1..64})"

# The mock ledger uses REAL server/migrations version strings throughout —
# never fabricated placeholders — so ledger_at_or_after (deploy-lib.sh),
# which resolves version order against the real on-disk file list, can
# actually locate them. real_migration_tail is the same "last 3" slice
# build_packet's own node script selects below; packet_minimum_rollback
# and packet_final_version are its first/last entries, used as the mock's
# default pre/post-migration ledger values so every scenario's ledger
# state is consistent with the packet it actually admitted.
read -r -a real_migration_tail <<<"$(node -e '
  const fs = require("fs");
  const files = fs.readdirSync("server/migrations").filter((f) => f.endsWith(".up.sql")).sort().slice(-3);
  console.log(files.map((f) => f.replace(/\.up\.sql$/, "")).join(" "));
')"
packet_minimum_rollback="${real_migration_tail[0]}"
packet_final_version="${real_migration_tail[2]}"
# A real version well before packet_minimum_rollback — the ledger state
# any scenario starts from before its first cutover has run.
pre_cutover_baseline="467_autopilot_trigger_creator_from_autopilot"

cat >"$mock_bin/node" <<MOCK
#!/usr/bin/env bash
set -euo pipefail
case "\$1" in
  */quiescence.mjs)
    control_dir="\${CUTOVER_TEST_CONTROL_DIR:?CUTOVER_TEST_CONTROL_DIR unset}"
    sub="\$2"
    printf '%s %s\n' "\$sub" "\$*" >>"\$control_dir/quiescence-calls.log"
    if [ -f "\$control_dir/deny-\$sub" ]; then
      echo '{"admit":false,"decision":"denied_test"}'
      exit 1
    fi
    echo '{"admit":true,"decision":"admitted_test"}'
    exit 0
    ;;
  *)
    exec "$real_node" "\$@"
    ;;
esac
MOCK
chmod +x "$mock_bin/node"

cat >"$mock_bin/docker" <<MOCK
#!/usr/bin/env bash
set -euo pipefail
control_dir="\${CUTOVER_TEST_CONTROL_DIR:?CUTOVER_TEST_CONTROL_DIR unset}"
log_file="\$control_dir/docker-calls.log"
printf '%s\n' "\$*" >>"\$log_file"

fail_if_flagged() {
  local flag=\$1
  if [ -f "\$control_dir/\$flag" ]; then
    echo "mock docker: forced failure (\$flag)" >&2
    exit 1
  fi
}

if [ "\$1" = "inspect" ]; then
  ref="\${*: -1}"
  repo="\${ref%%:*}"
  digest=""
  case "\$ref" in
    # sha-incumbent is running_image_ref's own mock tag (see the "images"
    # compose subcommand below) for the retained-predecessor colour's
    # ACTUAL running container — deliberately a different digest than the
    # candidate's own MOCK_BACKEND_DIGEST/MOCK_WEB_DIGEST, so a rollback
    # scenario copying the pre-rollback candidate tuple (CHE-678) instead
    # of resolving the real one is distinguishable in the written state.
    *multica-backend:sha-incumbent) digest="\${MOCK_INCUMBENT_BACKEND_DIGEST:-}" ;;
    *multica-web:sha-incumbent) digest="\${MOCK_INCUMBENT_WEB_DIGEST:-}" ;;
    *multica-backend:*) digest="\${MOCK_BACKEND_DIGEST:-}" ;;
    *multica-web:*) digest="\${MOCK_WEB_DIGEST:-}" ;;
  esac
  printf '%s@%s\n' "\$repo" "\$digest"
  exit 0
fi

if [ "\$1" = "run" ]; then
  # bare 'docker run' — used by router.sh's validate_config (nginx -t).
  # Always succeeds unless explicitly flagged, so cutover tests don't need
  # a real nginx image; test-router.sh already covers real nginx -t.
  fail_if_flagged fail-router-validate
  exit 0
fi

if [ "\$1" = "login" ]; then
  # login ghcr.io -u token --password-stdin — deploy-lib.sh's shared
  # ghcr_login (CHE-549), mirroring test-deploy.sh's own mock exactly so
  # both suites exercise the identical login/logout contract.
  password="\$(cat)"
  if [ -f "\$control_dir/fail-ghcr-login" ]; then
    printf 'login %s <rejected>\n' "\$2" >>"\$control_dir/registry-auth.log"
    echo "mock docker: forced failure (fail-ghcr-login)" >&2
    exit 1
  fi
  printf 'login %s password=%s\n' "\$2" "\$password" >>"\$control_dir/registry-auth.log"
  exit 0
fi

if [ "\$1" = "logout" ]; then
  printf 'logout %s\n' "\$2" >>"\$control_dir/registry-auth.log"
  exit 0
fi

if [ "\$1" = "exec" ] && [ "\$2" = "multica-ab-router" ]; then
  fail_if_flagged fail-router-reload
  exit 0
fi

if [ "\$1" = "compose" ]; then
  shift
  # shift off however many -f FILE pairs precede the subcommand
  while [ "\$1" = "-f" ]; do shift 2; done
  sub="\$1"; shift
  case "\$sub" in
    pull)
      fail_if_flagged fail-pull
      exit 0
      ;;
    run)
      # run --rm --no-deps --entrypoint ./migrate backend-<colour> up|down
      direction=""
      for a in "\$@"; do
        case "\$a" in
          up) direction="up" ;;
          down) direction="down" ;;
        esac
      done
      printf 'migrate %s image=%s tag=%s service=%s\n' \\
        "\$direction" "\${MULTICA_BACKEND_IMAGE:-}" "\${MULTICA_IMAGE_TAG:-}" "\${*: -2:1}" \\
        >>"\$control_dir/migrate-calls.log"
      fail_if_flagged fail-migrate
      # Genuinely advance the mock ledger on a real "up" run, so the
      # rollback compatibility guard is exercised against a schema state
      # that actually changed — not a static fixture value a test hand-
      # injects. "down" is intentionally NOT modeled here: routine rollback
      # never runs it (asserted by scenario 8/11 below); if this mock's
      # down branch is ever reached, that is itself a regression this test
      # should catch via the "no down migration" assertions.
      if [ "\$direction" = "up" ]; then
        printf '%s\n' "\${MOCK_LEDGER_VERSION_AFTER_MIGRATE:-$packet_final_version}" >"\$control_dir/ledger-version"
      fi
      exit 0
      ;;
    up)
      fail_if_flagged fail-container-start
      touch "\$control_dir/containers-up-\${*: -2:1}"
      exit 0
      ;;
    stop)
      printf '%s\n' "\$*" >>"\$control_dir/stop-calls.log"
      exit 0
      ;;
    images)
      # images <service> --format json — deploy-lib.sh's running_service_image,
      # asking what backend-<colour>/frontend-<colour> is ACTUALLY running.
      # Every colour reports the SAME fixed "sha-incumbent" tag here — this
      # mock never tracks per-colour state — which is enough to prove
      # rollback resolves the running container (via this + the inspect
      # mock's sha-incumbent digest above) rather than copying the
      # pre-rollback state file's own candidate tuple (CHE-678).
      case "\$1" in
        backend-*) printf '[{"Repository":"ghcr.io/cheese-work/multica-backend","Tag":"sha-incumbent"}]\n' ;;
        frontend-*) printf '[{"Repository":"ghcr.io/cheese-work/multica-web","Tag":"sha-incumbent"}]\n' ;;
        *) printf '[]\n' ;;
      esac
      exit 0
      ;;
    exec)
      # exec -T postgres psql ... ledger query
      version="\${MOCK_LEDGER_VERSION:-$pre_cutover_baseline}"
      if [ -f "\$control_dir/ledger-version" ]; then
        version="\$(cat "\$control_dir/ledger-version")"
      fi
      printf '%s\n' "\$version"
      exit 0
      ;;
    ps)
      # ps -q postgres — deploy-lib.sh's postgres_container_id, resolving
      # the running postgres container for quiescence.mjs's
      # --psql-via-docker-exec (CHE-655). A fixed fake id is enough: this
      # suite's mock node intercepts */quiescence.mjs entirely (see above)
      # and never actually execs into it.
      if [ "\$1" = "-q" ] && [ "\$2" = "postgres" ]; then
        printf 'mock-postgres-container-id\n'
        exit 0
      fi
      # ps --status running --format '{{.Service}}' backend frontend — the
      # base-service port-collision preflight. Empty (nothing running) by
      # default; a scenario touches base-services-running to model the
      # un-scaled base stack still being up.
      if [ -f "\$control_dir/base-services-running" ]; then
        printf 'backend\nfrontend\n'
      fi
      exit 0
      ;;
    *)
      echo "mock docker compose: unhandled subcommand \$sub" >&2
      exit 1
      ;;
  esac
fi

echo "mock docker: unhandled command \$*" >&2
exit 1
MOCK
chmod +x "$mock_bin/docker"

cat >"$mock_bin/curl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
control_dir="${CUTOVER_TEST_CONTROL_DIR:?CUTOVER_TEST_CONTROL_DIR unset}"
# Last argument is the URL; extract the port to know which colour is being probed.
url="${*: -1}"
port="$(printf '%s' "$url" | sed -E 's#^.*127\.0\.0\.1:([0-9]+).*#\1#')"
if [ -f "$control_dir/health-never-ready-$port" ]; then
  exit 22
fi
if printf '%s' "$url" | grep -q '/health$'; then
  if [ -f "$control_dir/health-commit-override-$port" ]; then
    cat "$control_dir/health-commit-override-$port"
  else
    printf '{"status":"ok","pid":1,"commit":"%s"}\n' "${MOCK_EXPECTED_COMMIT:-}"
  fi
  exit 0
fi
if [ -f "$control_dir/containers-up-backend-${port##1808}" ] || [ -f "$control_dir/ready-$port" ]; then
  exit 0
fi
# Default: ready once the corresponding "containers-up-backend-<colour>"
# marker exists for the colour whose port this is.
if [ -f "$control_dir/backend-ready-$port" ]; then
  exit 0
fi
exit 22
MOCK
chmod +x "$mock_bin/curl"

cat >"$mock_bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod +x "$mock_bin/sleep"

# flock is real (needed for the lock semantics); everything else is mocked.
export PATH="$mock_bin:$PATH"

compose_dir="$work_dir/compose"
mkdir -p "$compose_dir"
touch "$compose_dir/docker-compose.selfhost.yml"

manifest="$work_dir/manifest.json"
cat >"$manifest" <<JSON
{
  "images": {
    "backend": "ghcr.io/cheese-work/multica-backend@$backend_digest",
    "web": "ghcr.io/cheese-work/multica-web@$web_digest"
  },
  "source_sha": "$source_sha"
  ,"migration_inventory_sha256": "sha256:$(printf 'e%.0s' {1..64})"
  ,"baseline_tuple_sha256": "sha256:$(printf 'f%.0s' {1..64})"
}
JSON

build_packet() {
  local out=$1
  node -e '
    const fs = require("fs");
    const crypto = require("crypto");
    const path = require("path");
    const dir = "server/migrations";
    const files = fs.readdirSync(dir).filter((f) => f.endsWith(".up.sql")).sort();
    const last = files.slice(-3);
    const ordered = last.map((f) => {
      const bytes = fs.readFileSync(path.join(dir, f));
      return { version: f.replace(/\.up\.sql$/, ""), sha256: "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex") };
    });
    const packet = {
      schema_version: 1,
      candidate: {
        source_sha: "504078f8ea7fa31f342f195659e93a7f6c3e5a91",
        backend_image: "ghcr.io/cheese-work/multica-backend@sha256:" + "a".repeat(64),
        web_image: "ghcr.io/cheese-work/multica-web@sha256:" + "b".repeat(64),
        migration_inventory_sha256: "sha256:" + "e".repeat(64),
        baseline_tuple_sha256: "sha256:" + "f".repeat(64),
      },
      ordered_migrations: ordered,
      observed_state: {
        schema_version: 1,
        ledger: { status: "complete", versions: ordered.map((o) => o.version) },
        indexes: { status: "complete", invalid: [] },
        hooks: { status: "complete", observations: [{ name: "backfill_example", status: "completed" }] },
      },
      previous_image_pair: {
        backend: "ghcr.io/cheese-work/multica-backend:sha-abc123",
        web: "ghcr.io/cheese-work/multica-web:sha-abc123",
        backend_digest: "sha256:" + "a".repeat(64),
        web_digest: "sha256:" + "b".repeat(64),
      },
      minimum_rollback_version: ordered[0].version,
    };
    fs.writeFileSync(process.argv[1], JSON.stringify(packet, null, 2));
  ' "$out"
}
packet="$work_dir/packet.json"
build_packet "$packet"

expect_exit() {
  local want=$1 got=$2 name=$3
  if [ "$got" -ne "$want" ]; then
    echo "scenario $name: exit $got, want $want" >&2
    exit 1
  fi
}

expect_contains() {
  local haystack=$1 needle=$2 name=$3
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "scenario $name: output missing expected text: $needle" >&2
    echo "--- captured output ---" >&2
    echo "$haystack" >&2
    exit 1
  fi
}

expect_not_contains() {
  local haystack=$1 needle=$2 name=$3
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "scenario $name: output unexpectedly contains: $needle" >&2
    exit 1
  fi
}

fresh_scenario_dir() {
  local name=$1
  local dir="$work_dir/scenarios/$name"
  rm -rf "$dir"
  mkdir -p "$dir"
  printf '%s\n' "$dir"
}

run_cutover() {
  local state_dir=$1
  mkdir -p "$state_dir"
  export CUTOVER_TEST_CONTROL_DIR="$state_dir/control"
  mkdir -p "$CUTOVER_TEST_CONTROL_DIR"
  export MOCK_BACKEND_DIGEST="$backend_digest"
  export MOCK_WEB_DIGEST="$web_digest"
  export MOCK_INCUMBENT_BACKEND_DIGEST="$incumbent_backend_digest"
  export MOCK_INCUMBENT_WEB_DIGEST="$incumbent_web_digest"
  export MOCK_EXPECTED_COMMIT="$source_sha"
  export CUTOVER_DATABASE_URL="postgres://multica:multica@127.0.0.1:5432/multica?sslmode=disable"
  # ROUTER_STATE_DIR keeps router.sh's own state isolated per scenario
  # (cutover.sh's default otherwise points at the real repo-relative
  # deploy/cd/router/state, per the fix for independent-review finding 3's
  # second sub-finding — see cutover.sh's own comment on router_state_dir).
  export ROUTER_STATE_DIR="$state_dir/router-state"
  # Mark blue's backend as reachable/ready by default (the incumbent colour
  # in every scenario below); green becomes ready once its own containers-up
  # marker is touched by the mock's `up` handler.
  touch "$CUTOVER_TEST_CONTROL_DIR/backend-ready-${BACKEND_BLUE_PORT:-18081}"
  touch "$CUTOVER_TEST_CONTROL_DIR/backend-ready-${BACKEND_GREEN_PORT:-18082}"
  bash deploy/cd/cutover.sh cutover \
    --manifest "$manifest" \
    --packet "$packet" \
    --compose-dir "$compose_dir" \
    --state-dir "$state_dir"
}

# ---------------------------------------------------------------------------
# Scenario 1: happy path, first-ever cutover (no cutover-state.json) ->
# treats blue as incumbent, cuts over to green.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir happy-path)"
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
expect_exit 0 "$status" happy-path
expect_contains "$output" "cutover complete: green is now active" happy-path
if [ ! -f "$state_dir/cutover-state.json" ]; then
  echo "scenario happy-path: cutover-state.json was not written" >&2
  exit 1
fi
active="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).active_colour)' "$state_dir/cutover-state.json")"
if [ "$active" != "green" ]; then
  echo "scenario happy-path: active_colour is '$active', want green" >&2
  exit 1
fi

# Coverage gap (independent review): "no second active scheduler". Starting
# a backend also starts its in-process scheduler (CHE-393) — the structural
# guarantee this unit relies on is that cutover.sh's own control flow never
# has BOTH colours' backend containers running at once. Assert it directly
# against the actual sequence of `docker compose up`/`stop` calls this run
# issued: the candidate colour's `up` must be the only `up` for a backend
# service before the incumbent's `stop` for the same colour pairing is
# issued, and the incumbent must never be started again afterward. A
# structural proof (the real command sequence cutover.sh emitted), not an
# assumption from reading the code.
compose_backend_calls="$(grep -E 'compose .* (up -d --no-deps backend-|stop backend-)' "$state_dir/control/docker-calls.log" 2>/dev/null || true)"
up_count_blue="$(printf '%s\n' "$compose_backend_calls" | grep -cE 'up -d --no-deps backend-blue\b' || true)"
up_count_green="$(printf '%s\n' "$compose_backend_calls" | grep -cE 'up -d --no-deps backend-green\b' || true)"
if [ "$up_count_blue" -gt 0 ]; then
  echo "scenario happy-path: backend-blue (the incumbent) was started during a cutover to green — this is the exact 'second active scheduler' hazard the accepted architecture forbids" >&2
  echo "--- docker compose backend up/stop sequence ---" >&2
  printf '%s\n' "$compose_backend_calls" >&2
  exit 1
fi
if [ "$up_count_green" -ne 1 ]; then
  echo "scenario happy-path: expected exactly one 'up' for backend-green (the candidate), got $up_count_green" >&2
  exit 1
fi

# The controller must verify both immutable image digests before draining.
# Exercise each mismatch independently and assert no incumbent stop occurred.
for role in backend web; do
  state_dir="$(fresh_scenario_dir ${role}-digest-mismatch)"
  mkdir -p "$state_dir/control"
  export CUTOVER_TEST_CONTROL_DIR="$state_dir/control"
  export MOCK_BACKEND_DIGEST="$backend_digest"
  export MOCK_WEB_DIGEST="$web_digest"
  export MOCK_INCUMBENT_BACKEND_DIGEST="$incumbent_backend_digest"
  export MOCK_INCUMBENT_WEB_DIGEST="$incumbent_web_digest"
  export MOCK_EXPECTED_COMMIT="$source_sha"
  export CUTOVER_DATABASE_URL="postgres://multica:***@127.0.0.1:5432/multica?sslmode=disable"
  export ROUTER_STATE_DIR="$state_dir/router-state"
  if [ "$role" = backend ]; then
    export MOCK_BACKEND_DIGEST="sha256:$(printf 'e%.0s' {1..64})"
  else
    export MOCK_WEB_DIGEST="sha256:$(printf 'e%.0s' {1..64})"
  fi
  set +e
  output="$(bash deploy/cd/cutover.sh cutover --manifest "$manifest" --packet "$packet" --compose-dir "$compose_dir" --state-dir "$state_dir" 2>&1)"
  status=$?
  set -e
  expect_exit 1 "$status" "${role}-digest-mismatch"
  expect_contains "$output" "pulled ${role} image digest mismatch" "${role}-digest-mismatch"
  if [ -f "$state_dir/control/stop-calls.log" ]; then
    echo "scenario ${role}-digest-mismatch: incumbent was drained before both image digests were verified" >&2
    exit 1
  fi
done

# ---------------------------------------------------------------------------
# Scenario 2 (negative control): quiescence preflight denies -> cutover must
# refuse before touching Docker at all (no pull/migrate/start calls).
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir preflight-denied)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/deny-preflight"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" preflight-denied
expect_contains "$output" "quiescence preflight denied cutover" preflight-denied
if [ -f "$state_dir/control/docker-calls.log" ] && grep -q "^compose pull" "$state_dir/control/docker-calls.log"; then
  echo "scenario preflight-denied: docker compose pull was invoked despite preflight denial" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 3 (negative control): quiescence final-gate denies -> same
# refusal, one step later.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir final-gate-denied)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/deny-final-gate"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" final-gate-denied
expect_contains "$output" "quiescence final-gate denied cutover" final-gate-denied

# ---------------------------------------------------------------------------
# Scenario 4 (negative control): a release packet that fails contract
# checks (checksum drift) must refuse before any quiescence call.
# ---------------------------------------------------------------------------
bad_packet="$work_dir/bad-packet.json"
node -e '
  const fs = require("fs");
  const p = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  p.ordered_migrations[0].sha256 = "sha256:" + "f".repeat(64);
  fs.writeFileSync(process.argv[2], JSON.stringify(p));
' "$packet" "$bad_packet"
state_dir="$(fresh_scenario_dir bad-packet)"
mkdir -p "$state_dir/control"
export CUTOVER_TEST_CONTROL_DIR="$state_dir/control"
export MOCK_BACKEND_DIGEST="$backend_digest"
export CUTOVER_DATABASE_URL="postgres://multica:multica@127.0.0.1:5432/multica?sslmode=disable"
set +e
output="$(bash deploy/cd/cutover.sh cutover --manifest "$manifest" --packet "$bad_packet" --compose-dir "$compose_dir" --state-dir "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" bad-packet
expect_contains "$output" "release packet failed contract checks" bad-packet
if [ -f "$state_dir/control/quiescence-calls.log" ]; then
  echo "scenario bad-packet: quiescence was called despite a packet that should never reach it" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 4b (negative control, independent-review finding 4): the base
# (un-scaled) backend/frontend services are still running -> cutover must
# refuse before any mutation. The base frontend's published
# 127.0.0.1:${FRONTEND_PORT:-3000} collides with the router's own frontend
# listener; letting the cutover proceed would defer that failure to router
# activation, deep into the sequence, instead of catching it up front.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir base-services-still-running)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/base-services-running"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" base-services-still-running
expect_contains "$output" "base (pre-A/B) service(s) still running" base-services-still-running
if [ -f "$state_dir/control/quiescence-calls.log" ]; then
  echo "scenario base-services-still-running: quiescence was called despite the port-collision preflight that should have refused first" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 5 (negative control, CHE-655): migration step fails -> candidate
# never starts. The incumbent was ALREADY drained/stopped before the
# migration step ran (the fix: drain happens before the quiescence
# final-gate and migration, not after — see cutover.sh's own comment), so
# this is now an actual outage requiring manual intervention, not "blue
# remains active".
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir migrate-fails)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/fail-migrate"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" migrate-fails
expect_contains "$output" "migration step failed" migrate-fails
expect_contains "$output" "MANUAL INTERVENTION REQUIRED" migrate-fails
if ! grep -q "backend-blue" "$state_dir/control/stop-calls.log" 2>/dev/null; then
  echo "scenario migrate-fails: incumbent colour blue was NOT drained before the migration step ran — this is the CHE-655 ordering bug (quiescence final-gate/migration must never run before the incumbent is drained)" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 6 (negative control, CHE-655): candidate health check never
# becomes ready -> candidate stopped. The incumbent was already drained
# before the migration step ran, so the API is down and needs manual
# intervention, not "leaving blue active".
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir candidate-never-ready)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/health-never-ready-${BACKEND_GREEN_PORT:-18082}"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" candidate-never-ready
expect_contains "$output" "did not become ready within 180s" candidate-never-ready
expect_contains "$output" "MANUAL INTERVENTION REQUIRED" candidate-never-ready

# ---------------------------------------------------------------------------
# Scenario 7 (negative control): candidate /health reports the wrong commit
# (a stale process still bound to the port) -> refused, candidate stopped.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir wrong-health-identity)"
mkdir -p "$state_dir/control"
printf '{"status":"ok","pid":1,"commit":"some-other-sha"}\n' >"$state_dir/control/health-commit-override-${BACKEND_GREEN_PORT:-18082}"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" wrong-health-identity
expect_contains "$output" "health identity mismatch" wrong-health-identity

# ---------------------------------------------------------------------------
# Scenario 7b (negative control — coverage gap, "interrupted switch"): the
# candidate becomes healthy and passes /health identity, but the router
# reload itself fails mid-switch (config validated and symlinked, but the
# running Nginx process never picked it up — e.g. the container was killed
# or lost its connection at exactly that moment). cutover.sh must report
# this distinctly (candidate running but not receiving traffic) rather than
# claiming success. The incumbent was already drained before the quiescence
# final-gate/migration step ran (CHE-655 fix), so — unlike before that fix —
# it is expected to already be stopped by this point; what must never happen
# is claiming success (writing cutover-state.json) when the router never
# actually moved.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir interrupted-switch)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/fail-router-reload"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" interrupted-switch
expect_contains "$output" "router generation switch failed" interrupted-switch
expect_contains "$output" "NOT receiving traffic" interrupted-switch
if [ -f "$state_dir/cutover-state.json" ]; then
  echo "scenario interrupted-switch: cutover-state.json was written despite the switch never completing — this would claim green is active when the router never moved" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 7c (negative control — coverage gap, "lost controller
# connection"): a second cutover/rollback invocation holds the deploy lock
# (simulating a controller that is mid-run, or one whose connection was
# lost while still holding the lock) — this invocation must not proceed
# past lock acquisition, and must not touch Docker at all.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir lost-controller-connection)"
mkdir -p "$state_dir"
exec 8>"$state_dir/deploy.lock"
flock 8
set +e
output="$(CUTOVER_LOCK_WAIT_SECONDS=2 bash deploy/cd/cutover.sh cutover --manifest "$manifest" --packet "$packet" --compose-dir "$compose_dir" --state-dir "$state_dir" 2>&1)"
status=$?
set -e
exec 8>&-
expect_exit 1 "$status" lost-controller-connection
expect_contains "$output" "could not acquire deploy lock" lost-controller-connection
if [ -f "$state_dir/control/docker-calls.log" ]; then
  echo "scenario lost-controller-connection: Docker was invoked despite never acquiring the deploy lock" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 8 (proves the fix for independent-review finding 1, SECOND pass):
# rollback right after a cutover whose migration step GENUINELY advanced the
# ledger (the mock's default behavior — no test-injected value; ledger moves
# from pre_cutover_baseline to packet_final_version via a real "migrate up"
# mock invocation). Under expand/contract this is the NORMAL, routine
# rollback case — the retained predecessor is required to serve the
# expanded schema — so it must SUCCEED, not refuse.
#
# The independent review's first-pass fix compared the live ledger against
# the ledger recorded BEFORE the migration ran, which made this exact case
# refuse unconditionally — blocking rollback in precisely the situation A/B
# rollback exists for. The corrected guard compares the live ledger against
# the release packet's own minimum_rollback_version (order-aware, via
# ledger_at_or_after), which packet_final_version sits at or after by
# construction, so this now asserts SUCCESS — the inversion the review
# asked for.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir rollback-after-real-migration-cutover)"
run_cutover "$state_dir" >/dev/null 2>&1
set +e
output="$(bash deploy/cd/cutover.sh rollback --compose-dir "$compose_dir" --state-dir "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 0 "$status" rollback-after-real-migration-cutover
expect_contains "$output" "rollback complete: blue restored and healthy" rollback-after-real-migration-cutover
if grep -qE "migrate down|down --to" "$state_dir/control/docker-calls.log" "$state_dir/control/migrate-calls.log" 2>/dev/null; then
  echo "scenario rollback-after-real-migration-cutover: a down migration was run during routine rollback" >&2
  exit 1
fi
active="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).active_colour)' "$state_dir/cutover-state.json")"
if [ "$active" != "blue" ]; then
  echo "scenario rollback-after-real-migration-cutover: active_colour after rollback is '$active', want blue" >&2
  exit 1
fi

# Regression for CHE-678: cutover-state.json's image_tuple after this
# rollback must record what blue (the retained predecessor) is ACTUALLY
# running (the mock's "sha-incumbent" tag/incumbent_*_digest pair) — never
# the pre-rollback state file's own image_tuple, which still held green's
# (the failing candidate's) manifest-pinned digests. The bug this issue
# fixed copied the latter verbatim; assert both the correct value is
# present and the stale candidate value is not.
recorded_backend="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).image_tuple.backend)' "$state_dir/cutover-state.json")"
recorded_web="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).image_tuple.web)' "$state_dir/cutover-state.json")"
if [ "$recorded_backend" != "ghcr.io/cheese-work/multica-backend@$incumbent_backend_digest" ]; then
  echo "scenario rollback-after-real-migration-cutover: image_tuple.backend after rollback is '$recorded_backend', want the retained predecessor's actual running image (incumbent digest)" >&2
  exit 1
fi
if [ "$recorded_web" != "ghcr.io/cheese-work/multica-web@$incumbent_web_digest" ]; then
  echo "scenario rollback-after-real-migration-cutover: image_tuple.web after rollback is '$recorded_web', want the retained predecessor's actual running image (incumbent digest)" >&2
  exit 1
fi
if [[ "$recorded_backend" == *"$backend_digest"* ]] || [[ "$recorded_web" == *"$web_digest"* ]]; then
  echo "scenario rollback-after-real-migration-cutover: image_tuple still carries the failing candidate's own digest — rollback copied the pre-rollback state file instead of resolving what blue actually started from" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 9 (negative control): rollback with no cutover-state.json.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir rollback-no-state)"
mkdir -p "$state_dir"
set +e
output="$(bash deploy/cd/cutover.sh rollback --compose-dir "$compose_dir" --state-dir "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" rollback-no-state
expect_contains "$output" "nothing recorded to roll back from" rollback-no-state

# ---------------------------------------------------------------------------
# Scenario 10: a SECOND real cutover (green -> a new candidate on blue) runs
# its own genuine migration on top of the first cutover's already-advanced
# ledger, then rollback is attempted. Exercises the guard across two real
# hops rather than one — every ledger change here comes from an actual
# "migrate up" mock invocation triggered by a real cutover run, never a
# value hand-written into the rollback path. Each cutover records ITS OWN
# packet's minimum_rollback_version and advances the ledger to that same
# packet's final version, so the guard must still admit rollback after two
# hops, not just one — a real regression here (e.g. comparing against a
# stale floor from the first cutover instead of the second) would show up
# as this scenario failing.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir rollback-after-two-real-cutovers)"
run_cutover "$state_dir" >/dev/null 2>&1 # blue ($pre_cutover_baseline) -> green ($packet_final_version)
run_cutover "$state_dir" >/dev/null 2>&1 # green ($packet_final_version) -> blue-candidate ($packet_final_version)
active_after_two="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).active_colour)' "$state_dir/cutover-state.json")"
if [ "$active_after_two" != "blue" ]; then
  echo "scenario rollback-after-two-real-cutovers: active_colour after two cutovers is '$active_after_two', want blue" >&2
  exit 1
fi
set +e
output="$(bash deploy/cd/cutover.sh rollback --compose-dir "$compose_dir" --state-dir "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 0 "$status" rollback-after-two-real-cutovers
expect_contains "$output" "rollback complete: green restored and healthy" rollback-after-two-real-cutovers
if grep -qE "migrate down|down --to" "$state_dir/control/docker-calls.log" "$state_dir/control/migrate-calls.log" 2>/dev/null; then
  echo "scenario rollback-after-two-real-cutovers: a down migration was run during routine rollback" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 11 (negative control): the ledger sits BEHIND the recorded
# minimum_rollback_version floor — modeling an out-of-band down migration,
# backup restore, or corruption between the cutover and this rollback
# attempt. This state cannot arise from the mock's routine "migrate up"
# flow (mirroring production: routine cutover/rollback never runs a down
# migration either), so it is legitimately the one place this test writes
# the ledger-version control file directly — it is testing the fail-closed
# behavior for a state that is, by construction, only reachable out of
# band, not standing in for a real code path the mock could otherwise
# exercise (the distinction CHE-608's independent review drew for finding
# 2's original fake negative control).
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir rollback-ledger-behind-floor)"
run_cutover "$state_dir" >/dev/null 2>&1
echo "$pre_cutover_baseline" >"$state_dir/control/ledger-version"
set +e
output="$(bash deploy/cd/cutover.sh rollback --compose-dir "$compose_dir" --state-dir "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" rollback-ledger-behind-floor
expect_contains "$output" "not at or after the rollback compatibility floor" rollback-ledger-behind-floor
expect_contains "$output" "NOT a routine rollback case" rollback-ledger-behind-floor
if grep -qE "migrate down|down --to" "$state_dir/control/docker-calls.log" "$state_dir/control/migrate-calls.log" 2>/dev/null; then
  echo "scenario rollback-ledger-behind-floor: a down migration was run despite the guard refusing" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario 12 (CHE-549, carried from deploy.sh into the cutover pull path):
# GHCR_PULL_TOKEN set -> cutover.sh logs in to ghcr.io with the real token
# before pulling the candidate colour's images, and logs out on the normal
# success exit path.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir ghcr-login-happy-path)"
export GHCR_PULL_TOKEN=super-secret-pat
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
unset GHCR_PULL_TOKEN
expect_exit 0 "$status" ghcr-login-happy-path
expect_contains "$output" "logging in to ghcr.io" ghcr-login-happy-path

auth_log="$(cat "$state_dir/control/registry-auth.log")"
expect_contains "$auth_log" "login ghcr.io password=super-secret-pat" ghcr-login-happy-path
expect_contains "$auth_log" "logout ghcr.io" ghcr-login-happy-path
expect_not_contains "$output" "super-secret-pat" ghcr-login-happy-path

# ---------------------------------------------------------------------------
# Scenario 13 (CHE-549): GHCR login itself fails -> cutover refuses before
# any pull, incumbent colour remains active, and the EXIT trap still logs
# out even though the failure happened before the pull step ever ran.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir ghcr-login-fails)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/fail-ghcr-login"
export GHCR_PULL_TOKEN=super-secret-pat
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
unset GHCR_PULL_TOKEN
expect_exit 1 "$status" ghcr-login-fails
expect_contains "$output" "ghcr.io login failed" ghcr-login-fails
expect_contains "$output" "blue remains active" ghcr-login-fails

auth_log="$(cat "$state_dir/control/registry-auth.log")"
expect_not_contains "$auth_log" "login ghcr.io password=" ghcr-login-fails
expect_contains "$auth_log" "logout ghcr.io" ghcr-login-fails
if [ -f "$state_dir/control/docker-calls.log" ] && grep -q "^compose pull" "$state_dir/control/docker-calls.log"; then
  echo "scenario ghcr-login-fails: docker compose pull was invoked despite the ghcr.io login failing" >&2
  exit 1
fi

echo "cutover.sh control-flow fixtures passed"
