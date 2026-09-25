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
# What the pre-drain incumbent's /health reports — distinct from source_sha
# so recovery must prove it restarted the incumbent, not the candidate.
incumbent_commit="1111111111111111111111111111111111111111"

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
  # --format {{.Image}} <container id>: the image a RUNNING container was
  # created from (CHE-773 review finding 3). --format {{.Id}} <repo:tag>:
  # the local image that tag resolves to now. Kept distinct so a stale
  # container (image-<service> not refreshed by "up") is detectable.
  case "\$*" in
    *'{{.Image}}'*) cat "\$control_dir/image-\${ref#cid-}" 2>/dev/null; exit 0 ;;
    *'--format {{.Id}}'*) printf 'imgid-%s\n' "\$ref"; exit 0 ;;
  esac
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
  # nginx -T: the running router's effective config. Normally whatever
  # active.conf names; router-effective-conf models a router whose loaded
  # config disagrees with the generation files (CHE-773 review finding 4).
  if [ "\$4" = "-T" ]; then
    if [ -f "\$control_dir/router-effective-conf" ]; then
      cat "\$control_dir/router-effective-conf"
    else
      cat "\${ROUTER_STATE_DIR:?}/active.conf"
    fi
    exit 0
  fi
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
    config)
      # config --format json — the .env-aware render cutover.sh takes every
      # slot port from (CHE-773). Like real Compose, this reads ./.env from
      # the project directory itself (compose() cd's there) and ignores the
      # caller's unexported shell variables. The rendered map is also saved
      # for the curl mock, which has to know which service owns a port.
      render_port() {
        local var=\$1 default=\$2 value
        value="\$(sed -n "s/^\${var}=//p" .env 2>/dev/null | tail -1)"
        printf '%s' "\${value:-\$default}"
      }
      bb="\$(render_port BACKEND_BLUE_PORT 18081)"; bg="\$(render_port BACKEND_GREEN_PORT 18082)"
      fb="\$(render_port FRONTEND_BLUE_PORT 13001)"; fg="\$(render_port FRONTEND_GREEN_PORT 13002)"
      printf 'backend-blue=%s\nbackend-green=%s\nfrontend-blue=%s\nfrontend-green=%s\n' "\$bb" "\$bg" "\$fb" "\$fg" >"\$control_dir/rendered-ports"
      printf '{"services":{"backend-blue":{"ports":[{"target":8080,"published":"%s"}]},"backend-green":{"ports":[{"target":8080,"published":"%s"}]},"frontend-blue":{"ports":[{"target":3000,"published":"%s"}]},"frontend-green":{"ports":[{"target":3000,"published":"%s"}]}}}\n' "\$bb" "\$bg" "\$fb" "\$fg"
      exit 0
      ;;
    port)
      # port <service> <target> — the RUNNING container's actual binding.
      # published-port-<service> models a container whose binding differs
      # from the current render (e.g. created before .env changed).
      if [ -f "\$control_dir/published-port-\$1" ]; then
        printf '127.0.0.1:%s\n' "\$(cat "\$control_dir/published-port-\$1")"
      else
        printf '127.0.0.1:%s\n' "\$(sed -n "s/^\$1=//p" "\$control_dir/rendered-ports")"
      fi
      exit 0
      ;;
    up | start)
      [ "\$sub" = up ] && fail_if_flagged fail-container-start
      [ "\$sub" = start ] && printf '%s\n' "\$*" >>"\$control_dir/start-calls.log"
      for a in "\$@"; do
        case "\$a" in
          backend-* | frontend-*)
            # "up" (re)creates from the candidate image; "start" resumes the
            # stopped container, keeping whatever commit it last ran.
            if [ "\$sub" = up ] || [ ! -f "\$control_dir/commit-\$a" ]; then
              printf '%s\n' "\${MOCK_EXPECTED_COMMIT:-}" >"\$control_dir/commit-\$a"
            fi
            # "up" recreates the container from the tag it was given, unless
            # stale-container-<service> models Compose keeping an old one.
            if [ "\$sub" = up ] && [ ! -f "\$control_dir/stale-container-\$a" ]; then
              case "\$a" in
                backend-*) repo="\${MOCK_UP_BACKEND_REPO:-\${MULTICA_BACKEND_IMAGE:-}}" ;;
                frontend-*) repo="\${MULTICA_WEB_IMAGE:-}" ;;
              esac
              printf 'imgid-%s:%s\n' "\$repo" "\${MULTICA_IMAGE_TAG:-}" >"\$control_dir/image-\$a"
            fi
            touch "\$control_dir/running-\$a"
            ;;
        esac
      done
      exit 0
      ;;
    stop)
      # fail-stop-<service>: stop exits nonzero and the container keeps
      # running. stop-noop-<service>: stop exits 0 but the container is
      # still running (CHE-773 review findings 1/2).
      printf '%s\n' "\$*" >>"\$control_dir/stop-calls.log"
      status=0
      for a in "\$@"; do
        if [ -f "\$control_dir/fail-stop-\$a" ]; then
          status=1
        elif [ ! -f "\$control_dir/stop-noop-\$a" ]; then
          rm -f "\$control_dir/running-\$a"
        fi
      done
      exit "\$status"
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
      # ps -q <slot service>: its running container id.
      if [ "\$1" = "-q" ]; then
        [ -f "\$control_dir/running-\$2" ] && printf 'cid-%s\n' "\$2"
        exit 0
      fi
      # ps --status running --format ... <slot services>: which are running.
      for a in "\$@"; do
        case "\$a" in
          backend-* | frontend-*) [ -f "\$control_dir/running-\$a" ] && printf '%s\n' "\$a" ;;
        esac
      done
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
# Last argument is the URL. Resolve its port to the service that owns it —
# the router listeners (8081/3000) forward to whatever the ACTIVE router
# generation names, slot ports belong to whichever service Compose rendered
# them for — and answer only if that service is actually running. A probe on
# a port nothing publishes is connection-refused, exactly the CHE-773 shape.
url="${*: -1}"
port="$(printf '%s' "$url" | sed -E 's#^.*127\.0\.0\.1:([0-9]+).*#\1#')"
active_conf="${ROUTER_STATE_DIR:-}/active.conf"
case "$port" in
  8081 | 3000)
    # Route by the generation file Nginx actually includes, not by the
    # active.json metadata.
    [ -f "$active_conf" ] || exit 7
    port="$(awk -v l="$port" '/listen/ { hit = index($0, ":" l ";") > 0 } hit && /proxy_pass/ { match($0, /127\.0\.0\.1:[0-9]+/); print substr($0, RSTART + 10, RLENGTH - 10); exit }' "$active_conf")"
    ;;
esac
service="$(sed -n "s/=$port\$//p" "$control_dir/rendered-ports" 2>/dev/null | head -1)"
[ -n "$service" ] && [ -f "$control_dir/running-$service" ] || exit 7
[ -f "$control_dir/health-never-ready-$port" ] && exit 22
if printf '%s' "$url" | grep -q '/health$'; then
  if [ -f "$control_dir/health-commit-override-$port" ]; then
    cat "$control_dir/health-commit-override-$port"
  else
    printf '{"status":"ok","pid":%s,"commit":"%s","started_at":"t-%s"}\n' "${#service}$(printf '%s' "$service" | cksum | cut -c1-4)" "$(cat "$control_dir/commit-$service")" "$service"
  fi
fi
exit 0
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
  bootstrap_incumbent "${2:-$compose_dir}"
  bash deploy/cd/cutover.sh cutover \
    --manifest "$manifest" \
    --packet "$packet" \
    --compose-dir "${2:-$compose_dir}" \
    --state-dir "$state_dir"
}

# bootstrap_incumbent models a live host on first use of a scenario dir:
# blue running as commit incumbent_commit and the router selecting blue on
# the ports Compose renders for this compose dir. Later runs in the same
# dir keep whatever the previous cutover left behind.
bootstrap_incumbent() {
  local dir=$1
  [ -f "$ROUTER_STATE_DIR/active.json" ] && return 0
  (cd "$dir" && docker compose config --format json >/dev/null)
  local ports="$CUTOVER_TEST_CONTROL_DIR/rendered-ports"
  for svc in backend-blue frontend-blue; do
    touch "$CUTOVER_TEST_CONTROL_DIR/running-$svc"
    printf '%s\n' "$incumbent_commit" >"$CUTOVER_TEST_CONTROL_DIR/commit-$svc"
    printf 'imgid-incumbent-%s\n' "$svc" >"$CUTOVER_TEST_CONTROL_DIR/image-$svc"
  done
  BACKEND_BLUE_PORT="$(sed -n 's/^backend-blue=//p' "$ports")" \
    FRONTEND_BLUE_PORT="$(sed -n 's/^frontend-blue=//p' "$ports")" \
    bash deploy/cd/router.sh select --colour blue --state-dir "$ROUTER_STATE_DIR" >/dev/null
  : >"$CUTOVER_TEST_CONTROL_DIR/docker-calls.log"
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
expect_contains "$output" "did not become ready on port 18082 within 180s" candidate-never-ready
expect_contains "$output" "RECOVERED: cutover to green failed" candidate-never-ready

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

# ---------------------------------------------------------------------------
# Scenario 14: the reviewed controller runs from a disposable relocated
# bundle, but router activation must land in the durable directory mounted
# by the already-running router. Removing the bundle afterwards must neither
# remove nor invalidate the selected generation.
# ---------------------------------------------------------------------------
relocated_root="$work_dir/relocated-controller"
mkdir -p "$relocated_root/deploy/cd/router" "$relocated_root/server"
cp deploy/cd/{cutover.sh,deploy-lib.sh,docker-compose.ab.yml,quiescence.mjs,release-packet.mjs,router.sh} "$relocated_root/deploy/cd/"
cp deploy/cd/router/nginx.conf.template "$relocated_root/deploy/cd/router/"
cp -a server/migrations "$relocated_root/server/migrations"

state_dir="$(fresh_scenario_dir relocated-controller-live-router-state)"
export CUTOVER_TEST_CONTROL_DIR="$state_dir/control"
mkdir -p "$CUTOVER_TEST_CONTROL_DIR"
export MOCK_BACKEND_DIGEST="$backend_digest"
export MOCK_WEB_DIGEST="$web_digest"
export MOCK_INCUMBENT_BACKEND_DIGEST="$incumbent_backend_digest"
export MOCK_INCUMBENT_WEB_DIGEST="$incumbent_web_digest"
export MOCK_EXPECTED_COMMIT="$source_sha"
export CUTOVER_DATABASE_URL="postgres://multica:***@127.0.0.1:5432/multica?sslmode=disable"
durable_router_state="$work_dir/live-router-mount"
ROUTER_STATE_DIR="$durable_router_state" bootstrap_incumbent "$compose_dir"

remote_parent_script="$work_dir/remote-parent.sh"
GHCR_PULL_TOKEN='' bash deploy/cd/render-cutover-remote-script.sh \
  --router-state-dir "$durable_router_state" \
  -- bash "$relocated_root/deploy/cd/cutover.sh" cutover \
    --manifest "$manifest" --packet "$packet" --compose-dir "$compose_dir" --state-dir "$state_dir" \
  >"$remote_parent_script"

# A fresh shell is load-bearing: this must prove inheritance across the
# generated remote parent -> bundled child boundary, not reuse this test's
# own environment. Preserve only the mock controls the child genuinely needs.
output="$(env -i \
  HOME="$HOME" PATH="$PATH" \
  CUTOVER_TEST_CONTROL_DIR="$CUTOVER_TEST_CONTROL_DIR" \
  MOCK_BACKEND_DIGEST="$MOCK_BACKEND_DIGEST" MOCK_WEB_DIGEST="$MOCK_WEB_DIGEST" \
  MOCK_INCUMBENT_BACKEND_DIGEST="$MOCK_INCUMBENT_BACKEND_DIGEST" \
  MOCK_INCUMBENT_WEB_DIGEST="$MOCK_INCUMBENT_WEB_DIGEST" \
  MOCK_EXPECTED_COMMIT="$MOCK_EXPECTED_COMMIT" CUTOVER_DATABASE_URL="$CUTOVER_DATABASE_URL" \
  bash "$remote_parent_script" 2>&1)"
unset ROUTER_STATE_DIR
status=$?
expect_exit 0 "$status" relocated-controller-live-router-state
expect_contains "$output" "cutover complete: green is now active" relocated-controller-live-router-state
if [ ! -L "$durable_router_state/active.conf" ] || [ ! -L "$durable_router_state/active.json" ]; then
  echo "scenario relocated-controller-live-router-state: activation did not update the durable router mount" >&2
  exit 1
fi
if [ -e "$relocated_root/deploy/cd/router/state/active.conf" ]; then
  echo "scenario relocated-controller-live-router-state: activation leaked into disposable bundle-local router state" >&2
  exit 1
fi
route_colour="$(node -e 'process.stdout.write(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).colour)' "$durable_router_state/active.json")"
[ "$route_colour" = green ] || { echo "scenario relocated-controller-live-router-state: live router still selects $route_colour" >&2; exit 1; }
grep -q '127.0.0.1:18082' "$durable_router_state/active.conf" || {
  echo "scenario relocated-controller-live-router-state: live route does not target green backend" >&2
  exit 1
}
rm -rf "$relocated_root"
route_colour="$(node -e 'process.stdout.write(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).colour)' "$durable_router_state/active.json")"
[ "$route_colour" = green ] || { echo "scenario relocated-controller-live-router-state: bundle cleanup lost durable route" >&2; exit 1; }
grep -q '127.0.0.1:18082' "$durable_router_state/active.conf" || {
  echo "scenario relocated-controller-live-router-state: bundle cleanup invalidated durable route" >&2
  exit 1
}

# ---------------------------------------------------------------------------
# CHE-773 scenarios. The compose dir's .env overrides every slot port
# (C00's real values) and the controller is launched exactly the way
# cd-deploy.yml launches it: a `bash -c` that sources .env WITHOUT exporting
# it and then starts cutover.sh as a child. The fresh `env -i` shell keeps
# this test's own environment from leaking port variables into the child.
# ---------------------------------------------------------------------------
c00_compose_dir="$work_dir/compose-c00-ports"
mkdir -p "$c00_compose_dir"
touch "$c00_compose_dir/docker-compose.selfhost.yml"
printf 'BACKEND_BLUE_PORT=18091\nBACKEND_GREEN_PORT=18092\nFRONTEND_BLUE_PORT=13001\nFRONTEND_GREEN_PORT=13002\n' >"$c00_compose_dir/.env"

run_cd_shaped() {
  local state_dir=$1
  mkdir -p "$state_dir/control"
  export CUTOVER_TEST_CONTROL_DIR="$state_dir/control" ROUTER_STATE_DIR="$state_dir/router-state"
  bootstrap_incumbent "$c00_compose_dir"
  env -i HOME="$HOME" PATH="$PATH" \
    CUTOVER_TEST_CONTROL_DIR="$CUTOVER_TEST_CONTROL_DIR" ROUTER_STATE_DIR="$ROUTER_STATE_DIR" \
    MOCK_BACKEND_DIGEST="$backend_digest" MOCK_WEB_DIGEST="$web_digest" \
    MOCK_INCUMBENT_BACKEND_DIGEST="$incumbent_backend_digest" MOCK_INCUMBENT_WEB_DIGEST="$incumbent_web_digest" \
    MOCK_EXPECTED_COMMIT="$source_sha" MOCK_LEDGER_VERSION_AFTER_MIGRATE="${MOCK_LEDGER_VERSION_AFTER_MIGRATE:-}" \
    bash -c "cd '$c00_compose_dir' && . ./.env && export CUTOVER_DATABASE_URL=postgres://multica:multica@postgres:5432/multica && bash '$root_dir/deploy/cd/cutover.sh' cutover --manifest '$manifest' --packet '$packet' --compose-dir '$c00_compose_dir' --state-dir '$state_dir'"
}

json_field() {
  node -e 'process.stdout.write(String(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"))[process.argv[2]]))' "$1" "$2"
}

drained_before_refusal() {
  [ -f "$1/control/stop-calls.log" ] && grep -q "backend-blue" "$1/control/stop-calls.log"
}

# Happy path: health check, bindings and router all use 18092/13002.
state_dir="$(fresh_scenario_dir che773-nondefault-ports)"
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
expect_exit 0 "$status" che773-nondefault-ports
expect_contains "$output" "health-checking candidate colour=green on port 18092" che773-nondefault-ports
expect_contains "$output" "cutover complete: green is now active" che773-nondefault-ports
grep -q '127.0.0.1:18092' "$state_dir/router-state/active.conf" || { echo "scenario che773-nondefault-ports: router does not target 18092" >&2; exit 1; }
grep -q '127.0.0.1:13002' "$state_dir/router-state/active.conf" || { echo "scenario che773-nondefault-ports: router does not target 13002" >&2; exit 1; }
if grep -q '1808[12]' "$state_dir/router-state/active.conf"; then
  echo "scenario che773-nondefault-ports: router still carries a default 1808x port" >&2; exit 1
fi

# Pre-drain refusal 1: blue's RUNNING binding differs from the render
# (container created before .env changed). Nothing may be stopped.
state_dir="$(fresh_scenario_dir che773-incumbent-binding-mismatch)"
mkdir -p "$state_dir/control"
echo 18081 >"$state_dir/control/published-port-backend-blue"
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-incumbent-binding-mismatch
expect_contains "$output" "refusing before drain — blue remains active" che773-incumbent-binding-mismatch
if drained_before_refusal "$state_dir"; then echo "scenario che773-incumbent-binding-mismatch: blue was drained" >&2; exit 1; fi

# Pre-drain refusal 2: the router generation forwards to ports the render
# does not publish (the CHE-773 router/Compose split).
state_dir="$(fresh_scenario_dir che773-router-mismatch)"
mkdir -p "$state_dir/control"
export CUTOVER_TEST_CONTROL_DIR="$state_dir/control" ROUTER_STATE_DIR="$state_dir/router-state"
BACKEND_BLUE_PORT=18081 FRONTEND_BLUE_PORT=13001 bash deploy/cd/router.sh select --colour blue --state-dir "$ROUTER_STATE_DIR" >/dev/null
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-router-mismatch
expect_contains "$output" "router metadata selects colour=blue backend=18081" che773-router-mismatch
if drained_before_refusal "$state_dir"; then echo "scenario che773-router-mismatch: blue was drained" >&2; exit 1; fi

# Post-drain: candidate never ready -> gated recovery. Green is stopped
# BEFORE blue restarts (never two backends), blue comes back as its own
# pre-drain commit, and the public route reads back blue.
state_dir="$(fresh_scenario_dir che773-candidate-not-ready-recovers)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/health-never-ready-18092"
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-candidate-not-ready-recovers
expect_contains "$output" "did not become ready on port 18092" che773-candidate-not-ready-recovers
expect_contains "$output" "RECOVERED: cutover to green failed" che773-candidate-not-ready-recovers
expect_contains "$output" "commit $incumbent_commit" che773-candidate-not-ready-recovers
last_green_stop="$(grep -n 'stop backend-green' "$state_dir/control/docker-calls.log" | tail -1 | cut -d: -f1)"
blue_start="$(grep -n 'start backend-blue' "$state_dir/control/docker-calls.log" | tail -1 | cut -d: -f1)"
if [ -z "$last_green_stop" ] || [ -z "$blue_start" ] || [ "$last_green_stop" -gt "$blue_start" ]; then
  echo "scenario che773-candidate-not-ready-recovers: blue restarted before green was stopped (two backends)" >&2; exit 1
fi
[ -f "$state_dir/control/running-backend-blue" ] && [ ! -f "$state_dir/control/running-backend-green" ] || {
  echo "scenario che773-candidate-not-ready-recovers: want only blue running" >&2; exit 1; }
[ "$(json_field "$state_dir/router-state/active.json" colour)" = blue ] || {
  echo "scenario che773-candidate-not-ready-recovers: router not back on blue" >&2; exit 1; }
[ ! -f "$state_dir/cutover-state.json" ] || { echo "scenario che773-candidate-not-ready-recovers: cutover state recorded a failed cutover" >&2; exit 1; }

# Post-drain: candidate's actual published binding differs from the render.
state_dir="$(fresh_scenario_dir che773-candidate-binding-mismatch)"
mkdir -p "$state_dir/control"
echo 18082 >"$state_dir/control/published-port-backend-green"
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-candidate-binding-mismatch
expect_contains "$output" "published bindings differ from the rendered ports" che773-candidate-binding-mismatch
expect_contains "$output" "RECOVERED" che773-candidate-binding-mismatch

# Post-drain, unprovable compatibility: the ledger after migration is not
# a known version -> blue is NOT restarted blind; the outage is alerted
# loudly and recorded, never silent.
state_dir="$(fresh_scenario_dir che773-unprovable-recovery-alerts)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/health-never-ready-18092"
set +e
output="$(MOCK_LEDGER_VERSION_AFTER_MIGRATE=999999_unknown_out_of_band run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-unprovable-recovery-alerts
expect_contains "$output" "::error title=Multica A/B cutover outage::" che773-unprovable-recovery-alerts
expect_contains "$output" "NOT restarted" che773-unprovable-recovery-alerts
[ -f "$state_dir/outage-alert.json" ] || { echo "scenario che773-unprovable-recovery-alerts: no outage-alert.json" >&2; exit 1; }
if [ -f "$state_dir/control/start-calls.log" ]; then echo "scenario che773-unprovable-recovery-alerts: blue was started without compatibility proof" >&2; exit 1; fi

# Router without an explicit port must refuse, never fall back.
set +e
output="$(env -u BACKEND_GREEN_PORT -u FRONTEND_GREEN_PORT bash deploy/cd/router.sh validate --colour green --state-dir "$work_dir/router-no-port" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" router-no-port-fallback
expect_contains "$output" "BACKEND_GREEN_PORT is not set" router-no-port-fallback

# --- CHE-773 review (x99-codex-sol) negative controls ---------------------

no_green_started() {
  ! grep -qE 'up -d --no-deps backend-green' "$1/control/docker-calls.log"
}

# Finding 2: the drain of blue fails (stop exits nonzero) or silently leaves
# blue running (stop exits 0). Neither the final gate nor the migration may
# run, and green must not start.
for mode in fail-stop stop-noop; do
  name="che773-drain-$mode"
  state_dir="$(fresh_scenario_dir "$name")"
  mkdir -p "$state_dir/control"
  touch "$state_dir/control/$mode-backend-blue"
  set +e
  output="$(run_cd_shaped "$state_dir" 2>&1)"
  status=$?
  set -e
  expect_exit 1 "$status" "$name"
  expect_contains "$output" "drain of blue NOT confirmed" "$name"
  expect_contains "$output" "::error title=Multica A/B cutover outage::" "$name"
  if grep -q '^final-gate' "$state_dir/control/quiescence-calls.log" 2>/dev/null || [ -f "$state_dir/control/migrate-calls.log" ]; then
    echo "scenario $name: final gate/migration ran with blue possibly live" >&2; exit 1
  fi
  no_green_started "$state_dir" || { echo "scenario $name: green started beside blue" >&2; exit 1; }
done

# Finding 1: during recovery the candidate's stop fails or leaves it
# running. Blue must NOT be started (two schedulers on one DB); alert.
for mode in fail-stop stop-noop; do
  name="che773-recovery-candidate-$mode"
  state_dir="$(fresh_scenario_dir "$name")"
  mkdir -p "$state_dir/control"
  touch "$state_dir/control/health-never-ready-18092" "$state_dir/control/$mode-backend-green"
  set +e
  output="$(run_cd_shaped "$state_dir" 2>&1)"
  status=$?
  set -e
  expect_exit 1 "$status" "$name"
  expect_contains "$output" "candidate green shutdown NOT confirmed, so blue was NOT started" "$name"
  if [ -f "$state_dir/control/start-calls.log" ]; then
    echo "scenario $name: blue was started while green may still run" >&2; exit 1
  fi
done

# Finding 3a: Compose keeps a stale green container (old image) — the
# candidate must not be selected; recovery restores blue.
state_dir="$(fresh_scenario_dir che773-stale-candidate-container)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/stale-container-backend-green"
echo "imgid-ghcr.io/cheese-work/multica-backend:sha-old" >"$state_dir/control/image-backend-green"
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-stale-candidate-container
expect_contains "$output" "backend-green runs image 'imgid-ghcr.io/cheese-work/multica-backend:sha-old'" che773-stale-candidate-container
expect_contains "$output" "RECOVERED" che773-stale-candidate-container
[ ! -f "$state_dir/cutover-state.json" ] || { echo "scenario che773-stale-candidate-container: recorded a stale candidate as active" >&2; exit 1; }

# Finding 3b: the web container is stale while backend is fine.
state_dir="$(fresh_scenario_dir che773-stale-web-container)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/stale-container-frontend-green"
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-stale-web-container
expect_contains "$output" "frontend-green runs image" che773-stale-web-container
expect_contains "$output" "RECOVERED" che773-stale-web-container

# Finding 4a: active.json says blue on 18091, but active.conf (what Nginx
# includes) still forwards to a stale port -> refuse before drain.
state_dir="$(fresh_scenario_dir che773-router-conf-vs-metadata)"
mkdir -p "$state_dir/control"
export CUTOVER_TEST_CONTROL_DIR="$state_dir/control" ROUTER_STATE_DIR="$state_dir/router-state"
bootstrap_incumbent "$c00_compose_dir"
conf_target="$state_dir/router-state/$(readlink "$state_dir/router-state/active.conf")"
sed -i 's/127\.0\.0\.1:18091/127.0.0.1:18081/' "$conf_target"
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-router-conf-vs-metadata
expect_contains "$output" "router active.conf upstreams [8081=18081 3000=13001] differ from expected [8081=18091 3000=13001]" che773-router-conf-vs-metadata
if drained_before_refusal "$state_dir"; then echo "scenario che773-router-conf-vs-metadata: blue was drained" >&2; exit 1; fi

# Finding 4b: generation files agree but the RUNNING router loaded something
# else (nginx -T) -> refuse before drain.
state_dir="$(fresh_scenario_dir che773-router-effective-config)"
mkdir -p "$state_dir/control"
export CUTOVER_TEST_CONTROL_DIR="$state_dir/control" ROUTER_STATE_DIR="$state_dir/router-state"
bootstrap_incumbent "$c00_compose_dir"
sed 's/127\.0\.0\.1:18091/127.0.0.1:18081/' "$state_dir/router-state/active.conf" >"$state_dir/control/router-effective-conf"
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-router-effective-config
expect_contains "$output" "running router's effective config (nginx -T) upstreams [8081=18081 3000=13001]" che773-router-effective-config
if drained_before_refusal "$state_dir"; then echo "scenario che773-router-effective-config: blue was drained" >&2; exit 1; fi

# Finding 4c: after activation the running router keeps serving blue's
# ports (reload silently not applied). Equal commits can't hide this: the
# readback must fail, and recovery must restore blue.
state_dir="$(fresh_scenario_dir che773-router-not-applied-after-switch)"
mkdir -p "$state_dir/control"
export CUTOVER_TEST_CONTROL_DIR="$state_dir/control" ROUTER_STATE_DIR="$state_dir/router-state"
bootstrap_incumbent "$c00_compose_dir"
cp "$state_dir/router-state/active.conf" "$state_dir/control/router-effective-conf"
set +e
output="$(run_cd_shaped "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che773-router-not-applied-after-switch
expect_contains "$output" "running router's effective config (nginx -T) upstreams [8081=18091 3000=13001] differ from expected [8081=18092 3000=13002]" che773-router-not-applied-after-switch
expect_contains "$output" "RECOVERED" che773-router-not-applied-after-switch
unset ROUTER_STATE_DIR

echo "cutover.sh control-flow fixtures passed"
