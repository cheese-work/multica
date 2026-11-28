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

real_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

real_node="$(command -v node)"
real_mv="$(command -v mv)"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Scratch source root (CHE-7486 N5a): a copy of the real deploy/ and compose
# file (not symlinks: the .mjs main guards compare the resolved module path to
# argv[1]), and a copy of server/migrations with every contract marker line
# neutralized, so the scenarios below run against an unmarked tree and a
# packet without contract_migrations. The real tree's two marked fork
# conversions have no restore files yet; the N5a scenarios add their own
# marked file and restore stub to this root.
root_dir="$work_dir/root"
mkdir -p "$root_dir/server"
cp -r "$real_root/deploy" "$root_dir/deploy"
cp "$real_root/docker-compose.selfhost.yml" "$root_dir/docker-compose.selfhost.yml"
cp -r "$real_root/server/migrations" "$root_dir/server/migrations"
sed -i '1s/^-- multica:contract /-- unmarked in test-cutover: /' "$root_dir"/server/migrations/*.up.sql
cd "$root_dir"

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
# Image W (CHE-7486 N5v): what a candidate reference is moved to after one of
# its reads. It has digests the manifest never named.
w_backend_digest="sha256:$(printf '9%.0s' {1..64})"
w_web_digest="sha256:$(printf '8%.0s' {1..64})"
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
    [ "\$sub" != final-gate ] || echo final-gate >>"\$control_dir/events.log"
    # CHE-9367: a scenario can script the exact stdout and exit status of a
    # sub-command (output-<sub>, exit-<sub>) instead of the admit/deny pair.
    if [ -f "\$control_dir/output-\$sub" ]; then
      cat "\$control_dir/output-\$sub"
      exit "\$(cat "\$control_dir/exit-\$sub" 2>/dev/null || echo 1)"
    fi
    if [ -f "\$control_dir/deny-\$sub" ]; then
      echo '{"admit":false,"decision":"denied_test"}'
      exit 1
    fi
    echo '{"admit":true,"decision":"admitted_test"}'
    exit 0
    ;;
  */contract-snapshot.mjs)
    # CHE-7486 N5a: tamper-copy changes a copy file between its manifest and
    # the controller's fresh SHA-256 check (a corrupt or edited snapshot).
    if [ "\$2" = verify ] && [ -f "\${CUTOVER_TEST_CONTROL_DIR:-/nonexistent}/tamper-copy" ]; then
      printf 'x' >>"\$(dirname "\$4")/n5a_alpha.copy"
    fi
    exec "$real_node" "\$@"
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
flags_overlay=false
case "\$*" in *docker-compose.flags.yml*) flags_overlay=true ;; esac
printf '%s\n' "\$*" >>"\$log_file"

fail_if_flagged() {
  local flag=\$1
  if [ -f "\$control_dir/\$flag" ]; then
    echo "mock docker: forced failure (\$flag)" >&2
    exit 1
  fi
}

# Image and reference tables (CHE-7486 N5v). A candidate reference
# (multica-backend:<tag> or multica-web:<tag>, not the incumbent's tag) points
# at image imgid-<ref> until it is moved. Every inspect, image inspect,
# compose run and compose up resolves a reference once per call and counts one
# read (reads-<role>). MOCK_REPOINT_AFTER_READ=<n> moves the candidate backend
# reference to image W (imgid-W-backend) right after its n-th read;
# MOCK_REPOINT_REF=web moves the web reference instead. The read itself
# still answers with the image it resolved before the move.
resolve_ref() {
  local ref=\$1 role n id
  case "\$ref" in
    imgid-*) printf '%s\n' "\$ref"; return 0 ;; # already an image id
    *multica-backend:sha-incumbent | *multica-web:sha-incumbent) printf 'imgid-%s\n' "\$ref"; return 0 ;;
    *multica-backend:*) role=backend ;;
    *multica-web:*) role=web ;;
    *) printf 'imgid-%s\n' "\$ref"; return 0 ;;
  esac
  n=\$(( \$(cat "\$control_dir/reads-\$role" 2>/dev/null || echo 0) + 1 ))
  echo "\$n" >"\$control_dir/reads-\$role"
  id="\$(cat "\$control_dir/ref-\$role" 2>/dev/null || printf 'imgid-%s' "\$ref")"
  if [ "\${MOCK_REPOINT_AFTER_READ:-}" = "\$n" ] && [ "\${MOCK_REPOINT_REF:-backend}" = "\$role" ]; then
    echo "imgid-W-\$role" >"\$control_dir/ref-\$role"
  fi
  printf '%s\n' "\$id"
}
# w_digest_for_id prints image W's digest when the id is W's, else nothing.
w_digest_for_id() {
  case "\$1" in
    imgid-W-backend) printf '%s' "$w_backend_digest" ;;
    imgid-W-web) printf '%s' "$w_web_digest" ;;
  esac
}

if [ "\$1" = "image" ] && [ "\$2" = "inspect" ]; then
  # image inspect --format '{{.Id}}{{range .RepoDigests}} {{.}}{{end}}' <ref>:
  # the id and the digest list from one read (verified_image_id).
  ref="\${*: -1}"
  case "\$*" in
    *'{{.Id}}{{range .RepoDigests}} {{.}}{{end}}'*) ;;
    *) echo "mock docker: unhandled image inspect \$*" >&2; exit 1 ;;
  esac
  id="\$(resolve_ref "\$ref")"
  repo="\${ref%%:*}"
  case "\$ref" in
    *multica-backend:sha-incumbent) digest="\${MOCK_INCUMBENT_BACKEND_DIGEST:-}" ;;
    *multica-web:sha-incumbent) digest="\${MOCK_INCUMBENT_WEB_DIGEST:-}" ;;
    *multica-backend:*) digest="\${MOCK_BACKEND_DIGEST:-}" ;;
    *) digest="\${MOCK_WEB_DIGEST:-}" ;;
  esac
  [ -z "\$(w_digest_for_id "\$id")" ] || digest="\$(w_digest_for_id "\$id")"
  # no-repodigests: a local image that was never pulled has an empty list.
  if [ -f "\$control_dir/no-repodigests" ]; then
    printf '%s\n' "\$id"
  else
    printf '%s %s@%s\n' "\$id" "\$repo" "\$digest"
  fi
  exit 0
fi

if [ "\$1" = "inspect" ]; then
  ref="\${*: -1}"
  id=""
  case "\$ref" in cid-*) ;; *) id="\$(resolve_ref "\$ref")" ;; esac
  # --format {{.Image}} <container id>: the image a RUNNING container was
  # created from (CHE-773 review finding 3). --format {{.Id}} <repo:tag>:
  # the local image that tag resolves to now. Kept distinct so a stale
  # container (image-<service> not refreshed by "up") is detectable.
  case "\$*" in
    *'{{json .Mounts}}'*)
      if [ -f "\$control_dir/flags-mount-\${ref#cid-}" ]; then
        printf '[{"Destination":"/app/feature-flags.yaml"}]\n'
      else
        printf '[]\n'
      fi
      exit 0 ;;
    *'{{.Image}}'*) cat "\$control_dir/image-\${ref#cid-}" 2>/dev/null; exit 0 ;;
    *'--format {{.Id}}'*) printf '%s\n' "\$id"; exit 0 ;;
    # Drift receipt (CHE-2396): env key names of a container (cid-<service>)
    # or of its image, and the image's repo digests. env-missing-/env-extra-
    # <service> and running-digest-<backend|web> model drifted containers.
    *'{{json .Config.Env}}'*)
      env='"PATH=/usr/bin"'
      case "\$ref" in
        cid-frontend-*) [ -f "\$control_dir/env-missing-\${ref#cid-}" ] || env="\$env,\"MULTICA_APP_URL=\"" ;;
      esac
      case "\$ref" in
        cid-*) [ -f "\$control_dir/env-extra-\${ref#cid-}" ] && env="\$env,\"STALE_FLAG=1\"" ;;
      esac
      printf '[%s]\n' "\$env"; exit 0 ;;
    *'{{json .RepoDigests}}'*)
      case "\$ref" in
        *multica-backend*) role=backend; repo=ghcr.io/cheese-work/multica-backend; digest="\${MOCK_BACKEND_DIGEST:-}" ;;
        *) role=web; repo=ghcr.io/cheese-work/multica-web; digest="\${MOCK_WEB_DIGEST:-}" ;;
      esac
      [ -f "\$control_dir/running-digest-\$role" ] && digest="\$(cat "\$control_dir/running-digest-\$role")"
      [ -z "\$(w_digest_for_id "\$id")" ] || digest="\$(w_digest_for_id "\$id")"
      printf '["%s@%s"]\n' "\$repo" "\$digest"; exit 0 ;;
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
  [ -z "\$(w_digest_for_id "\$id")" ] || digest="\$(w_digest_for_id "\$id")"
  # no-repodigests: real docker fails on {{index .RepoDigests 0}} of an empty list.
  [ ! -f "\$control_dir/no-repodigests" ] || exit 1
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

if [ "\$1" = "network" ] && [ "\$2" = "inspect" ]; then
  # network inspect <name> — the configured Jev gateway network must already
  # exist (CHE-1854); missing-gateway-network models one that does not.
  fail_if_flagged missing-gateway-network
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
      # fail-pull-times N: fail only the next N pulls, modeling a transient
      # registry reset (CHE-823, run 36527988890) that a retry recovers.
      if [ -f "\$control_dir/fail-pull-times" ]; then
        left="\$(cat "\$control_dir/fail-pull-times")"
        if [ "\$left" -gt 0 ]; then
          echo \$((left - 1)) >"\$control_dir/fail-pull-times"
          echo "mock docker: failed to copy: read: connection reset by peer" >&2
          exit 1
        fi
      fi
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
      passed=no
      prev=""
      for a in "\$@"; do
        [ "\$prev" = -e ] && [ "\$a" = MULTICA_CONTRACT_ATTEMPT ] && passed=yes
        prev="\$a"
      done
      printf 'migrate-run direction=%s attempt=%s passed=%s\n' "\$direction" "\${MULTICA_CONTRACT_ATTEMPT:-}" "\$passed" >>"\$control_dir/events.log"
      printf 'migrate %s\n' "\$(resolve_ref "\${MULTICA_BACKEND_IMAGE:-}:\${MULTICA_IMAGE_TAG:-}")" >>"\$control_dir/run-image-ids.log"
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
      printf '{"services":{"backend-blue":{"ports":[{"target":8080,"published":"%s"}]},"backend-green":{"ports":[{"target":8080,"published":"%s"}]},"frontend-blue":{"environment":{"MULTICA_APP_URL":""},"ports":[{"target":3000,"published":"%s"}]},"frontend-green":{"environment":{"MULTICA_APP_URL":""},"ports":[{"target":3000,"published":"%s"}]}}}\n' "\$bb" "\$bg" "\$fb" "\$fg"
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
            if [ "\$sub" = up ] && [[ "\$a" = backend-* ]]; then
              if [ "\$flags_overlay" = true ]; then
                touch "\$control_dir/flags-mount-\$a"
              else
                rm -f "\$control_dir/flags-mount-\$a"
              fi
            fi
            # "up" (re)creates from the candidate image; "start" resumes the
            # stopped container, keeping whatever commit it last ran.
            if [ "\$sub" = up ] || [ ! -f "\$control_dir/commit-\$a" ]; then
              printf '%s\n' "\${MOCK_EXPECTED_COMMIT:-}" >"\$control_dir/commit-\$a"
            fi
            # "up" recreates the container from the tag it was given, unless
            # stale-container-<service> models Compose keeping an old one.
            if [ "\$sub" = up ]; then
              case "\$a" in
                backend-*) repo="\${MOCK_UP_BACKEND_REPO:-\${MULTICA_BACKEND_IMAGE:-}}" ;;
                frontend-*) repo="\${MULTICA_WEB_IMAGE:-}" ;;
              esac
              up_id="\$(resolve_ref "\$repo:\${MULTICA_IMAGE_TAG:-}")"
              if [ ! -f "\$control_dir/stale-container-\$a" ]; then
                printf '%s\n' "\$up_id" >"\$control_dir/image-\$a"
                printf 'up %s %s\n' "\$a" "\$up_id" >>"\$control_dir/run-image-ids.log"
              fi
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
      # CHE-7486 N5a: the snapshot gate's queries and COPY go to one script.
      case "\$*" in
        *"COPY "*|*information_schema*|*to_regclass*|*"FROM schema_contracts"*|*"WHERE version"*|*"count(*)"*)
          exec "$mock_bin/mock-psql-contract" "\$@" ;;
      esac
      # exec -T postgres psql ... ledger query
      version="\${MOCK_LEDGER_VERSION:-$pre_cutover_baseline}"
      if [ -f "\$control_dir/ledger-version" ]; then
        version="\$(cat "\$control_dir/ledger-version")"
      fi
      printf '%s\n' "\$version"
      exit 0
      ;;
    ps)
      if [ "\$1" = "--all" ] && [ "\$2" = "-q" ]; then
        if [ -f "\$control_dir/running-\$3" ] || [ -f "\$control_dir/flags-mount-\$3" ]; then
          printf 'cid-%s\n' "\$3"
        fi
        exit 0
      fi
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

cat >"$mock_bin/mock-psql-contract" <<'MOCK'
#!/usr/bin/env bash
# Scripted psql for the contract snapshot gate (CHE-7486 N5a). The default
# COPY text holds a tab, an escaped newline, an escaped backslash, \N and
# non-ASCII text, one table row per line.
set -euo pipefail
control_dir="${CUTOVER_TEST_CONTROL_DIR:?CUTOVER_TEST_CONTROL_DIR unset}"
sql="${*: -1}"
copy_text() {
  if [ -f "$control_dir/copy-$1" ]; then cat "$control_dir/copy-$1"; else
    printf '1\talpha\\nwrapped\t\\N\n2\tbeta \\\\ slash\tcaf\xc3\xa9\n'
  fi
}
table_of() { sed -E "s/.*(COPY|to_regclass\('|FROM|table_name = ')[[:space:]]*([a-z_0-9]+).*/\2/" <<<"$sql"; }
version_of() { sed -E "s/.*version = '([^']+)'.*/\1/" <<<"$sql"; }
case "$sql" in
  *"COPY "*)
    t="$(table_of)"
    echo "psql-copy $t" >>"$control_dir/events.log"
    if [ -f "$control_dir/fail-copy-$t" ]; then
      copy_text "$t" | head -n 1
      echo "mock psql: forced COPY failure ($t)" >&2
      exit 1
    fi
    copy_text "$t" ;;
  *"to_regclass('schema_contracts')"*)
    if [ -f "$control_dir/schema-contracts-table" ]; then echo t; else echo f; fi ;;
  *"to_regclass("*)
    if [ -f "$control_dir/absent-table-$(table_of)" ]; then echo f; else echo t; fi ;;
  *"FROM schema_contracts"*)
    if [ -f "$control_dir/contract-row-$(version_of)" ]; then echo 1; else echo 0; fi ;;
  *"FROM schema_migrations WHERE version"*)
    if [ -f "$control_dir/ledger-row-$(version_of)" ]; then echo 1; else echo 0; fi ;;
  *"count(*) FROM schema_migrations"*) echo 535 ;;
  *information_schema*) printf 'id\nname\nnote\n' ;;
  *"count(*) FROM "*)
    t="$(table_of)"
    if [ -f "$control_dir/rows-$t" ]; then cat "$control_dir/rows-$t"; else copy_text "$t" | wc -l | tr -d ' '; fi ;;
  *) echo "mock psql: unhandled SQL: $sql" >&2; exit 1 ;;
esac
MOCK
chmod +x "$mock_bin/mock-psql-contract"

# mv: record the attempt record's rename; fail-attempt-rename fails it.
cat >"$mock_bin/mv" <<MOCK
#!/usr/bin/env bash
dest="\${*: -1}"
case "\$dest" in
  *contract-attempts/*.json)
    echo rename-attempt >>"\${CUTOVER_TEST_CONTROL_DIR:?}/events.log"
    if [ -f "\$CUTOVER_TEST_CONTROL_DIR/fail-attempt-rename" ]; then
      echo "mock mv: forced rename failure" >&2
      exit 1
    fi ;;
esac
exec "$real_mv" "\$@"
MOCK
chmod +x "$mock_bin/mv"

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
    port="$(awk -v l="$port" '/listen/ { hit = index($0, ":" l ";") > 0 } hit && /proxy_pass[[:space:]]/ { match($0, /127\.0\.0\.1:[0-9]+/); print substr($0, RSTART + 10, RLENGTH - 10); exit }' "$active_conf")"
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
if [[ "$url" = */api/config ]]; then
  printf '%s\n' "$url" >>"$control_dir/config-calls.log"
  [ ! -f "$control_dir/config-unavailable-$service" ] || exit 22
  if [ -f "$control_dir/config-$service" ]; then
    cat "$control_dir/config-$service"
  else
    printf '{"feature_flags":{"project_workflows_v1":false,"plugins_v1":false,"composio_mcp_apps":false,"billing_workspace_subscriptions":false}}\n'
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
if [ "${CUTOVER_FLAGS_GUARD_ONLY:-0}" != 1 ]; then
state_dir="$(fresh_scenario_dir che1593-privacy-location-upstreams)"
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
expect_exit 0 "$status" che1593-privacy-location-upstreams
expect_contains "$output" "cutover complete: green is now active" che1593-privacy-location-upstreams
expect_contains "$output" "==> candidate image ids: backend imgid-ghcr.io/" che1593-privacy-location-upstreams
expect_contains "$output" "multica-backend:sha-${source_sha}, web imgid-ghcr.io/" che1593-privacy-location-upstreams
expect_contains "$output" "multica-web:sha-${source_sha} (digest-verified)" che1593-privacy-location-upstreams
if [ ! -f "$state_dir/cutover-state.json" ]; then
  echo "scenario che1593-privacy-location-upstreams: cutover-state.json was not written" >&2
  exit 1
fi
active="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).active_colour)' "$state_dir/cutover-state.json")"
if [ "$active" != "green" ]; then
  echo "scenario che1593-privacy-location-upstreams: active_colour is '$active', want green" >&2
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

# An image with no RepoDigests entry (never pulled from a registry) exits 1
# before the drain, as today.
state_dir="$(fresh_scenario_dir n5v-no-repodigests)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/no-repodigests"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" n5v-no-repodigests
expect_contains "$output" "pulled backend image digest mismatch" n5v-no-repodigests
if [ -f "$state_dir/control/stop-calls.log" ]; then
  echo "scenario n5v-no-repodigests: incumbent was drained for an image with no RepoDigests entry" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# CHE-7486 N5v: the image id the controller checks and starts is the id of the
# image whose digest matched the manifest. A candidate reference moved to
# another image (W) right after the read that verified the digest must never
# end in a reported success with W serving. The mock counts one read per
# inspect, image inspect, compose run and compose up (MOCK_REPOINT_AFTER_READ,
# MOCK_REPOINT_REF in the docker mock above).
# ---------------------------------------------------------------------------
# n5v_run <scenario> [VAR=value ...]: one cutover with the given mock settings.
# Sets n5v_dir, n5v_output and n5v_status.
n5v_run() {
  local name=$1
  shift
  n5v_dir="$(fresh_scenario_dir "$name")"
  set +e
  n5v_output="$(
    for assignment in "$@"; do export "$assignment"; done
    run_cutover "$n5v_dir" 2>&1
  )"
  n5v_status=$?
  set -e
}

# The cutover never reports success while a container created from W exists,
# and a container from W is refused by the image-id check itself
# (runs_verified_image), not left for the later drift receipt to catch after
# W passed health checks.
n5v_expect_no_success_with_w() {
  local name=$1
  if grep -qs 'imgid-W' "$n5v_dir"/control/image-*; then
    if [[ "$n5v_output" == *"cutover complete"* ]]; then
      echo "scenario $name: cutover reported success with a container from image W" >&2
      echo "--- captured output ---" >&2
      echo "$n5v_output" >&2
      exit 1
    fi
    if [[ "$n5v_output" != *"-green runs image 'imgid-W-"* ]]; then
      echo "scenario $name: a container from image W was not refused by the image-id check" >&2
      echo "--- captured output ---" >&2
      echo "$n5v_output" >&2
      exit 1
    fi
  fi
}

for ref in backend web; do
  name="n5v-boundary-$ref"
  n5v_run "$name" MOCK_REPOINT_AFTER_READ=1 MOCK_REPOINT_REF="$ref"
  expect_exit 1 "$n5v_status" "$name"
  expect_contains "$n5v_output" "-green runs image 'imgid-W-$ref'" "$name"
  expect_contains "$n5v_output" "RECOVERED" "$name"
  expect_not_contains "$n5v_output" "drift receipt" "$name"
  expect_not_contains "$n5v_output" "cutover complete" "$name"
  n5v_expect_no_success_with_w "$name"
done

# Sweep: move the reference after every read a run without a move makes.
n5v_run n5v-reads-baseline
expect_exit 0 "$n5v_status" n5v-reads-baseline
expect_contains "$n5v_output" "cutover complete: green is now active" n5v-reads-baseline
for ref in backend web; do
  reads="$(cat "$n5v_dir/control/reads-$ref")"
  if [ "$reads" -lt 2 ]; then
    echo "scenario n5v-sweep-$ref: a run without a move made only $reads reads of the $ref reference" >&2
    exit 1
  fi
  for n in $(seq 1 "$reads"); do
    name="n5v-sweep-$ref-$n"
    n5v_run "$name" MOCK_REPOINT_AFTER_READ="$n" MOCK_REPOINT_REF="$ref"
    n5v_expect_no_success_with_w "$name"
  done
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
# CHE-9367 (CHE-9223 S1): the preflight's exit status is a signal. Only a
# healthy `deferred_contention` denial (admit=false, nothing drained) exits 75,
# so cd-deploy.yml can mark the run as deferred. Everything else stays 1:
# an unreadable database, a crash, no or bad JSON, a different decision, an
# admit=true line, and the final-gate denials after drain (CHE-6850).
# ---------------------------------------------------------------------------
preflight_exit_case() {
  # name, want status, sub-command, stdout of the mock, exit status of the mock
  local name=$1 want=$2 sub=$3 out=$4 code=$5
  state_dir="$(fresh_scenario_dir "$name")"
  mkdir -p "$state_dir/control"
  printf '%s' "$out" >"$state_dir/control/output-$sub"
  printf '%s\n' "$code" >"$state_dir/control/exit-$sub"
  set +e
  output="$(run_cutover "$state_dir" 2>&1)"
  status=$?
  set -e
  expect_exit "$want" "$status" "$name"
  if [ "$sub" = preflight ]; then
    expect_contains "$output" "quiescence preflight denied cutover" "$name"
  else
    expect_contains "$output" "quiescence final-gate denied cutover" "$name"
  fi
  if [ -f "$state_dir/control/docker-calls.log" ] && grep -q "^compose pull" "$state_dir/control/docker-calls.log"; then
    echo "scenario $name: docker compose pull was invoked despite the denial" >&2
    exit 1
  fi
}
preflight_exit_case preflight-deferred-contention-exit-75 75 preflight \
  '{"admit":false,"decision":"deferred_contention","reason":"a foreign advisory-lock holder is present","evidence":{}}
' 1
preflight_exit_case preflight-observation-failed-exit-1 1 preflight \
  '{"admit":false,"decision":"observation_failed","reason":"observation failed: psql timed out","evidence":{}}
' 1
preflight_exit_case preflight-crash-exit-1 1 preflight '' 2
preflight_exit_case preflight-no-json-exit-1 1 preflight 'node: command crashed
' 1
preflight_exit_case preflight-bad-json-exit-1 1 preflight '{"admit":false,"decision":"deferred_contention"' 1
preflight_exit_case preflight-json-array-exit-1 1 preflight '[{"admit":false,"decision":"deferred_contention"}]
' 1
preflight_exit_case preflight-admit-true-deferred-exit-1 1 preflight \
  '{"admit":true,"decision":"deferred_contention"}
' 1
preflight_exit_case preflight-other-decision-exit-1 1 preflight \
  '{"admit":false,"decision":"final_gate_denied","reason":"x","evidence":{}}
' 1
# A deferred_contention-shaped denial from the FINAL gate (after drain) is a
# real failure, never a deferral.
preflight_exit_case final-gate-deferred-shape-exit-1 1 final-gate \
  '{"admit":false,"decision":"deferred_contention","reason":"x","evidence":{}}
' 1
echo "preflight exit-status scenarios passed"

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
cp deploy/cd/{contract-snapshot.mjs,cutover.sh,deploy-lib.sh,docker-compose.ab.yml,docker-compose.flags.yml,docker-compose.jev.yml,drift-receipt.mjs,quiescence.mjs,release-packet.mjs,router.sh} "$relocated_root/deploy/cd/"
cp docker-compose.selfhost.yml "$relocated_root/"
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

# ---------------------------------------------------------------------------
# CHE-823: one GHCR connection reset during `compose pull` failed a whole
# admitted deploy (run 36527988890). A transient pull failure must be
# retried; a persistent one must still fail closed after a bounded number
# of attempts, before drain, with the incumbent colour active.
# ---------------------------------------------------------------------------
pull_calls() { grep -c "^compose .*pull" "$1/control/docker-calls.log" || true; }

state_dir="$(fresh_scenario_dir che823-pull-transient-reset)"
mkdir -p "$state_dir/control"
echo 2 >"$state_dir/control/fail-pull-times"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 0 "$status" che823-pull-transient-reset
expect_contains "$output" "image pull attempt 1/3 failed" che823-pull-transient-reset
expect_contains "$output" "cutover complete: green is now active" che823-pull-transient-reset
if [ "$(pull_calls "$state_dir")" -ne 3 ]; then
  echo "scenario che823-pull-transient-reset: want 3 pull calls, got $(pull_calls "$state_dir")" >&2
  exit 1
fi

state_dir="$(fresh_scenario_dir che823-pull-persistent-failure)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/fail-pull"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" che823-pull-persistent-failure
expect_contains "$output" "image pull failed for colour=green; blue remains active" che823-pull-persistent-failure
if [ "$(pull_calls "$state_dir")" -ne 3 ]; then
  echo "scenario che823-pull-persistent-failure: want 3 pull calls, got $(pull_calls "$state_dir")" >&2
  exit 1
fi
if drained_before_refusal "$state_dir"; then echo "scenario che823-pull-persistent-failure: blue was drained" >&2; exit 1; fi

# ---------------------------------------------------------------------------
# CHE-1854: scoped Jev runtime inputs survive recreation. The optional
# deploy/cd/docker-compose.flags.yml carries flags independently of Jev.
# The Jev overlay is added only when the compose dir's .env configures its network; a configured-but-broken
# prerequisite refuses before the first mutation (drain, pull, migrate).
# Launched CD-shaped (.env sourced, not exported) with synthetic values only.
# ---------------------------------------------------------------------------
fi

jev_dir="$work_dir/compose-jev"
mkdir -p "$jev_dir"
touch "$jev_dir/docker-compose.selfhost.yml"
jev_key="$(printf 'A%.0s' {1..32} | base64)"
jev_flags="$work_dir/jev-flags.yaml"
cp deploy/cd/fixtures/jev-feature-flags.synthetic.yaml "$jev_flags"

write_jev_env() { # write_jev_env [KEY=VALUE ...] -> .env holding exactly these lines
  : >"$jev_dir/.env"
  local kv
  for kv in "$@"; do printf '%s\n' "$kv" >>"$jev_dir/.env"; done
}
jev_full_env() {
  write_jev_env "MULTICA_JEV_SECRET_KEY=$jev_key" "MULTICA_JEV_FLAGS_FILE=$jev_flags" "MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway" "$@"
}

run_jev() { # run_jev <state_dir> <cutover|rollback>
  local state_dir=$1 command=$2 extra=()
  [ "$command" = cutover ] && extra=(--manifest "$manifest" --packet "$packet")
  mkdir -p "$state_dir/control"
  export CUTOVER_TEST_CONTROL_DIR="$state_dir/control" ROUTER_STATE_DIR="$state_dir/router-state"
  bootstrap_incumbent "$jev_dir"
  env -i HOME="$HOME" PATH="$PATH" \
    CUTOVER_TEST_CONTROL_DIR="$CUTOVER_TEST_CONTROL_DIR" ROUTER_STATE_DIR="$ROUTER_STATE_DIR" \
    MOCK_BACKEND_DIGEST="$backend_digest" MOCK_WEB_DIGEST="$web_digest" \
    MOCK_INCUMBENT_BACKEND_DIGEST="$incumbent_backend_digest" MOCK_INCUMBENT_WEB_DIGEST="$incumbent_web_digest" \
    MOCK_EXPECTED_COMMIT="$source_sha" MOCK_LEDGER_VERSION_AFTER_MIGRATE="${MOCK_LEDGER_VERSION_AFTER_MIGRATE:-}" \
    bash -c "cd '$jev_dir' && . ./.env && export CUTOVER_DATABASE_URL=postgres://multica:multica@postgres:5432/multica && bash '$root_dir/deploy/cd/cutover.sh' $command ${extra[*]:-} --compose-dir '$jev_dir' --state-dir '$state_dir'"
}

jev_overlay_calls() { grep -c 'docker-compose.jev.yml' "$1/control/docker-calls.log" || true; }
flags_overlay_calls() { grep -c 'docker-compose.flags.yml' "$1/control/docker-calls.log" || true; }
compose_calls() { grep -c '^compose ' "$1/control/docker-calls.log" || true; }
mutated() { [ -f "$1/control/stop-calls.log" ] || grep -qE '^compose .*(pull|up|run) ' "$1/control/docker-calls.log"; }

if [ "${CUTOVER_FLAGS_GUARD_ONLY:-0}" != 1 ]; then
for flags_setting in MULTICA_FEATURE_FLAGS_FILE MULTICA_JEV_FLAGS_FILE; do
  write_jev_env "$flags_setting=$jev_flags"
  state_dir="$(fresh_scenario_dir "flags-only-$flags_setting")"
  output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
  expect_exit 0 "$status" "flags-only-$flags_setting"
  if [ "$(flags_overlay_calls "$state_dir")" -ne "$(compose_calls "$state_dir")" ] || [ "$(compose_calls "$state_dir")" -eq 0 ]; then
    echo "scenario flags-only-$flags_setting: generic flags overlay missing from compose calls" >&2
    exit 1
  fi
  [ "$(jev_overlay_calls "$state_dir")" -eq 0 ] || { echo "scenario flags-only-$flags_setting: Jev overlay loaded without a gateway" >&2; exit 1; }
  output="$(run_jev "$state_dir" rollback 2>&1)" && status=0 || status=$?
  expect_exit 0 "$status" "flags-rollback-$flags_setting"
  grep -E 'up -d --no-deps backend-blue\b' "$state_dir/control/docker-calls.log" | grep -q 'docker-compose.flags.yml' || { echo "scenario flags-rollback-$flags_setting: predecessor restarted without flags" >&2; exit 1; }
done

write_jev_env "MULTICA_FEATURE_FLAGS_FILE=$jev_flags" "MULTICA_JEV_FLAGS_FILE=$work_dir/absent.yaml"
state_dir="$(fresh_scenario_dir flags-generic-precedence)"
output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
expect_exit 0 "$status" flags-generic-precedence

write_jev_env "MULTICA_FEATURE_FLAGS_FILE=$root_dir/deploy/cd/fixtures/flags/invalid/duplicate-key.yaml"
state_dir="$(fresh_scenario_dir flags-malformed)"
output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
expect_exit 1 "$status" flags-malformed
expect_contains "$output" MULTICA_FEATURE_FLAGS_FILE flags-malformed
if mutated "$state_dir"; then echo "scenario flags-malformed: mutated before refusing" >&2; exit 1; fi

# Unconfigured: byte-identical compose arguments, no overlay.
write_jev_env
state_dir="$(fresh_scenario_dir jev-unconfigured)"
output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
expect_exit 0 "$status" jev-unconfigured
expect_contains "$output" "cutover complete: green is now active" jev-unconfigured
[ "$(jev_overlay_calls "$state_dir")" -eq 0 ] || { echo "scenario jev-unconfigured: overlay used without configuration" >&2; exit 1; }
[ "$(flags_overlay_calls "$state_dir")" -eq 0 ] || { echo "scenario jev-unconfigured: flags overlay used without configuration" >&2; exit 1; }

# Configured: the overlay rides on every compose call; one candidate up, the
# incumbent never started; the key never reaches output or docker arguments.
jev_full_env
state_dir="$(fresh_scenario_dir jev-configured)"
output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
expect_exit 0 "$status" jev-configured
expect_contains "$output" "cutover complete: green is now active" jev-configured
if [ "$(jev_overlay_calls "$state_dir")" -ne "$(compose_calls "$state_dir")" ] || [ "$(compose_calls "$state_dir")" -eq 0 ]; then
  echo "scenario jev-configured: overlay missing from $(( $(compose_calls "$state_dir") - $(jev_overlay_calls "$state_dir") )) compose call(s)" >&2
  exit 1
fi
[ "$(flags_overlay_calls "$state_dir")" -eq "$(compose_calls "$state_dir")" ] || { echo "scenario jev-configured: generic flags overlay missing" >&2; exit 1; }
[ "$(grep -cE 'up -d --no-deps backend-green\b' "$state_dir/control/docker-calls.log")" -eq 1 ] || { echo "scenario jev-configured: want exactly one backend-green up" >&2; exit 1; }
if grep -qE 'up -d --no-deps backend-blue\b' "$state_dir/control/docker-calls.log"; then
  echo "scenario jev-configured: incumbent backend-blue was started (two active backends)" >&2; exit 1
fi
expect_not_contains "$output" "$jev_key" jev-configured
if grep -qF "$jev_key" "$state_dir/control/docker-calls.log"; then echo "scenario jev-configured: key reached docker arguments" >&2; exit 1; fi

# .env quoting, `export` and trailing comments resolve like Compose's.
write_jev_env "export MULTICA_JEV_SECRET_KEY=\"$jev_key\"" "MULTICA_JEV_FLAGS_FILE='$jev_flags'  # scoped file" "MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway # gateway"
state_dir="$(fresh_scenario_dir jev-quoted-env)"
output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
expect_exit 0 "$status" jev-quoted-env
expect_contains "$output" "jev overlay enabled" jev-quoted-env
jev_full_env

# Rollback restarts the predecessor through the same overlay.
set +e
output="$(run_jev "$state_dir" rollback 2>&1)"
status=$?
set -e
expect_exit 0 "$status" jev-rollback
expect_contains "$output" "rollback complete: blue restored and healthy" jev-rollback
grep -E 'up -d --no-deps backend-blue\b' "$state_dir/control/docker-calls.log" | grep -q 'docker-compose.jev.yml' || { echo "scenario jev-rollback: predecessor restarted without the overlay" >&2; exit 1; }

# Missing / invalid configured prerequisites refuse before any mutation, name
# the variable and never print its value. (name | env lines | control flag)
jev_refusals=(
  "MULTICA_JEV_SECRET_KEY|MULTICA_JEV_FLAGS_FILE=$jev_flags,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_SECRET_KEY|MULTICA_JEV_SECRET_KEY=not-base64-$jev_key,MULTICA_JEV_FLAGS_FILE=$jev_flags,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_SECRET_KEY|MULTICA_JEV_SECRET_KEY=$(printf 'A%.0s' {1..31} | base64),MULTICA_JEV_FLAGS_FILE=$jev_flags,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=$work_dir/absent.yaml,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=$work_dir,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=relative.yaml,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=$root_dir/deploy/cd/fixtures/flags/invalid/duplicate-key.yaml,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=$root_dir/deploy/cd/fixtures/flags/invalid/unclosed-flow.yaml,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=$root_dir/deploy/cd/fixtures/flags/invalid/flag-not-map.yaml,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=$root_dir/deploy/cd/fixtures/flags/invalid/percent-overflow-by-one.yaml,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_FLAGS_FILE|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=$root_dir/deploy/cd/fixtures/flags/invalid/percent-overflow-uint64.yaml,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|"
  "MULTICA_JEV_GATEWAY_NETWORK|MULTICA_JEV_SECRET_KEY=$jev_key,MULTICA_JEV_FLAGS_FILE=$jev_flags,MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway|missing-gateway-network"
)
n=0
for spec in "${jev_refusals[@]}"; do
  n=$((n + 1))
  IFS='|' read -r var lines flag <<<"$spec"
  IFS=',' read -r -a env_lines <<<"$lines"
  write_jev_env "${env_lines[@]}"
  state_dir="$(fresh_scenario_dir "jev-refusal-$n")"
  mkdir -p "$state_dir/control"
  [ -z "$flag" ] || touch "$state_dir/control/$flag"
  output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
  expect_exit 1 "$status" "jev-refusal-$n"
  expect_contains "$output" "overlay" "jev-refusal-$n"
  expect_contains "$output" "$var" "jev-refusal-$n"
  expect_not_contains "$output" "$jev_key" "jev-refusal-$n"
  if mutated "$state_dir"; then echo "scenario jev-refusal-$n: mutated before refusing ($var)" >&2; exit 1; fi
done

# A rollback whose prerequisites broke since the cutover refuses before it
# drains the active colour.
jev_full_env
state_dir="$(fresh_scenario_dir jev-rollback-refusal)"
run_jev "$state_dir" cutover >/dev/null 2>&1
write_jev_env "MULTICA_JEV_FLAGS_FILE=$jev_flags" "MULTICA_JEV_GATEWAY_NETWORK=synthetic_gateway"
stops_before="$(wc -l <"$state_dir/control/stop-calls.log")"
output="$(run_jev "$state_dir" rollback 2>&1)" && status=0 || status=$?
expect_exit 1 "$status" jev-rollback-refusal
expect_contains "$output" "MULTICA_JEV_SECRET_KEY" jev-rollback-refusal
[ "$(wc -l <"$state_dir/control/stop-calls.log")" -eq "$stops_before" ] || { echo "scenario jev-rollback-refusal: drained before refusing" >&2; exit 1; }

# A flag file that turns malformed after a successful cutover refuses the
# rollback before it drains the active colour (both colours mount the file).
jev_full_env
state_dir="$(fresh_scenario_dir jev-rollback-malformed-file)"
run_jev "$state_dir" cutover >/dev/null 2>&1
cp "$root_dir/deploy/cd/fixtures/flags/invalid/duplicate-key.yaml" "$jev_flags"
stops_before="$(wc -l <"$state_dir/control/stop-calls.log")"
output="$(run_jev "$state_dir" rollback 2>&1)" && status=0 || status=$?
expect_exit 1 "$status" jev-rollback-malformed-file
expect_contains "$output" "MULTICA_JEV_FLAGS_FILE" jev-rollback-malformed-file
[ "$(wc -l <"$state_dir/control/stop-calls.log")" -eq "$stops_before" ] || { echo "scenario jev-rollback-malformed-file: drained before refusing" >&2; exit 1; }
cp deploy/cd/fixtures/jev-feature-flags.synthetic.yaml "$jev_flags"

# ---------------------------------------------------------------------------
# CHE-2396 drift gate. A stale compose dir is overwritten from the released
# source before anything renders; any env-key or image-digest difference on
# the started candidate fails the cutover before the router switches.
# ---------------------------------------------------------------------------
state_dir="$(fresh_scenario_dir drift-stale-compose-synced)"
stale_dir="$work_dir/stale-compose"
rm -rf "$stale_dir"; mkdir -p "$stale_dir/deploy/cd"
printf 'stale base\n' >"$stale_dir/docker-compose.selfhost.yml"
printf 'stale overlay\n' >"$stale_dir/deploy/cd/docker-compose.ab.yml"
output="$(run_cutover "$state_dir" "$stale_dir" 2>&1)" && status=0 || status=$?
expect_exit 0 "$status" drift-stale-compose-synced
for f in docker-compose.selfhost.yml deploy/cd/docker-compose.ab.yml deploy/cd/docker-compose.flags.yml deploy/cd/docker-compose.jev.yml; do
  cmp -s "$root_dir/$f" "$stale_dir/$f" || { echo "scenario drift-stale-compose-synced: $f was not synced from the released source" >&2; exit 1; }
done
[ "$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).status)' "$state_dir/drift-receipt.json")" = match ] \
  || { echo "scenario drift-stale-compose-synced: no match receipt" >&2; exit 1; }
# The interpolated compose render holds secrets: it must never be persisted.
[ -z "$(find "$state_dir" -name 'compose-config*' 2>/dev/null)" ] \
  || { echo "scenario drift-stale-compose-synced: compose render was persisted in the state dir" >&2; exit 1; }

drift_case() {
  local name=$1 flag=$2 content=$3 want=$4
  state_dir="$(fresh_scenario_dir "$name")"
  mkdir -p "$state_dir/control"
  printf '%s' "$content" >"$state_dir/control/$flag"
  output="$(run_cutover "$state_dir" 2>&1)" && status=0 || status=$?
  expect_exit 1 "$status" "$name"
  expect_contains "$output" "$want" "$name"
  [ ! -f "$state_dir/cutover-state.json" ] || { echo "scenario $name: success state written despite drift" >&2; exit 1; }
  [ "$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).status)' "$state_dir/drift-receipt.json")" = drift ] \
    || { echo "scenario $name: no drift receipt" >&2; exit 1; }
}
drift_case drift-env-key-missing env-missing-frontend-green "" "frontend-green env: missing key MULTICA_APP_URL"
drift_case drift-env-key-unexpected env-extra-backend-green "" "backend-green env: unexpected key STALE_FLAG"
drift_case drift-image-digest running-digest-web "sha256:$(printf 'e%.0s' {1..64})" "frontend-green image: running digest"

# ---------------------------------------------------------------------------
# CHE-7486 N5a: contract migrations. A marked incoming migration makes the
# controller snapshot its tables and write an attempt record AFTER the
# quiescence final-gate and BEFORE the migrator, and hand the attempt id to
# the migrator. The last real migration is marked for these scenarios, with
# a packet that lists it, a restore stub in the scratch tree and a ledger
# that lacks it. contract_end puts the unmarked tree and packet back.
# ---------------------------------------------------------------------------
contract_version="$packet_final_version"
contract_file="server/migrations/${contract_version}.up.sql"
contract_restore="deploy/cd/n5a-restore-fixture.sh"
unmarked_packet="$packet"

contract_begin() {
  cp "$contract_file" "$work_dir/contract-file.orig"
  { printf -- '-- multica:contract snapshot=n5a_alpha,n5a_beta restore=%s\n' "$contract_restore"; cat "$work_dir/contract-file.orig"; } >"$contract_file"
  printf '#!/usr/bin/env bash\necho restore fixture\n' >"$contract_restore"
  packet="$work_dir/contract-packet.json"
  build_packet "$packet"
  node -e '
    const fs = require("fs");
    const p = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    p.observed_state.ledger.versions = p.ordered_migrations.slice(0, 2).map((m) => m.version);
    p.contract_migrations = [{ version: process.argv[2], snapshot_tables: ["n5a_alpha", "n5a_beta"], restore: process.argv[3] }];
    fs.writeFileSync(process.argv[1], JSON.stringify(p, null, 2));
  ' "$packet" "$contract_version" "$contract_restore"
}

contract_end() {
  cp "$work_dir/contract-file.orig" "$contract_file"
  rm -f "$contract_restore" "$work_dir/contract-file.orig"
  packet="$unmarked_packet"
}

# contract_fails NAME CONTROL-FILE WANT-TEXT: one failing gate step must run no
# migrator, restart the incumbent and leave no success state.
contract_fails() {
  local name=$1 control=$2 want=$3
  local dir
  dir="$(fresh_scenario_dir "$name")"
  mkdir -p "$dir/control"
  for c in $control; do touch "$dir/control/$c"; done
  set +e
  output="$(run_cutover "$dir" 2>&1)"
  status=$?
  set -e
  expect_exit 1 "$status" "$name"
  expect_contains "$output" "$want" "$name"
  expect_contains "$output" "RECOVERED:" "$name"
  if grep -q '^migrate-run' "$dir/control/events.log" 2>/dev/null; then
    echo "scenario $name: the migrator ran although the contract gate failed" >&2
    exit 1
  fi
  [ ! -f "$dir/cutover-state.json" ] || { echo "scenario $name: success state written" >&2; exit 1; }
  [ -f "$dir/control/running-backend-blue" ] || { echo "scenario $name: incumbent blue is not running" >&2; exit 1; }
  [ ! -f "$dir/control/running-backend-green" ] || { echo "scenario $name: candidate green is running" >&2; exit 1; }
}

contract_begin

state_dir="$(fresh_scenario_dir n5a-contract-success)"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 0 "$status" n5a-contract-success
events="$state_dir/control/events.log"
attempt_id="$(sed -n 's/^migrate-run direction=up attempt=\([0-9a-f]\{32\}\) passed=yes$/\1/p' "$events")"
if [ -z "$attempt_id" ]; then
  echo "scenario n5a-contract-success: the migrator did not run with a 32-hex MULTICA_CONTRACT_ATTEMPT passed through -e" >&2
  cat "$events" >&2 || true
  exit 1
fi
got_order="$(grep -E '^(final-gate|psql-copy|rename-attempt|migrate-run)' "$events" | sed 's/attempt=[0-9a-f]*/attempt=ID/')"
want_order="$(printf '%s\n' 'final-gate' 'psql-copy n5a_alpha' 'psql-copy n5a_beta' 'rename-attempt' 'migrate-run direction=up attempt=ID passed=yes')"
if [ "$got_order" != "$want_order" ]; then
  echo "scenario n5a-contract-success: wrong order of final-gate, snapshot, attempt record and migrator" >&2
  echo "--- got ---" >&2; echo "$got_order" >&2
  echo "--- want ---" >&2; echo "$want_order" >&2
  exit 1
fi
snap_dir="$state_dir/contract-snapshots/$attempt_id"
record="$state_dir/contract-attempts/$attempt_id.json"
[ "$(stat -c %a "$snap_dir")" = 700 ] || { echo "scenario n5a-contract-success: snapshot dir mode is not 0700" >&2; exit 1; }
for f in "$snap_dir"/*; do
  [ "$(stat -c %a "$f")" = 600 ] || { echo "scenario n5a-contract-success: $f mode is not 0600" >&2; exit 1; }
done
[ "$(stat -c %a "$record")" = 600 ] || { echo "scenario n5a-contract-success: attempt record mode is not 0600" >&2; exit 1; }
node -e '
  const fs = require("fs");
  const crypto = require("crypto");
  const [snap, recordPath, id, version, sha] = process.argv.slice(1);
  const m = JSON.parse(fs.readFileSync(`${snap}/manifest.json`, "utf8"));
  const r = JSON.parse(fs.readFileSync(recordPath, "utf8"));
  const want = Buffer.from("1\talpha\\nwrapped\t\\N\n2\tbeta \\\\ slash\tcafé\n");
  const check = (ok, what) => { if (!ok) { console.error("scenario n5a-contract-success: " + what); process.exit(1); } };
  check(m.attempt_id === id && m.source_sha === sha, "manifest attempt_id or source_sha");
  check(m.tables.length === 2 && m.tables.every((t) => t.contract_version === version && t.rows === 2 && t.columns.join() === "id,name,note"), "manifest tables");
  check(fs.readFileSync(`${snap}/n5a_alpha.copy`).equals(want), "COPY bytes were not kept verbatim");
  check(m.tables[0].sha256 === "sha256:" + crypto.createHash("sha256").update(want).digest("hex"), "manifest sha256");
  check(typeof m.taken_at === "string" && m.ledger && typeof m.ledger.latest === "string", "manifest taken_at or ledger");
  check(r.attempt_id === id && r.source_sha === sha && r.snapshot_dir === snap && r.contract_versions.join() === version, "record identity fields");
  check(r.manifest_sha256 === "sha256:" + crypto.createHash("sha256").update(fs.readFileSync(`${snap}/manifest.json`)).digest("hex"), "record manifest_sha256");
  check(/^sha256:[0-9a-f]{64}$/.test(r.candidate_backend_digest) && /^sha256:[0-9a-f]{64}$/.test(r.incumbent_backend_digest), "record digests");
  check(typeof r.incumbent_backend_image_id === "string" && r.incumbent_backend_image_id !== "" && typeof r.written_at === "string", "record image id or written_at");
' "$snap_dir" "$record" "$attempt_id" "$contract_version" "$source_sha"
snapshot_before="$(sha256sum "$snap_dir/manifest.json" "$record")"

# A second attempt (green back to blue) writes a new id, directory and record
# and keeps the first.
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 0 "$status" n5a-contract-second-attempt
[ "$(ls "$state_dir/contract-snapshots" | wc -l)" -eq 2 ] || { echo "scenario n5a-contract-second-attempt: want 2 snapshot dirs" >&2; exit 1; }
[ "$(ls "$state_dir/contract-attempts" | wc -l)" -eq 2 ] || { echo "scenario n5a-contract-second-attempt: want 2 attempt records" >&2; exit 1; }
[ "$(sha256sum "$snap_dir/manifest.json" "$record")" = "$snapshot_before" ] || { echo "scenario n5a-contract-second-attempt: the first attempt was changed" >&2; exit 1; }
[ "$(grep -c '^migrate-run direction=up attempt=[0-9a-f]\{32\} ' "$events")" -eq 2 ] || { echo "scenario n5a-contract-second-attempt: both runs need an attempt id" >&2; exit 1; }
[ "$(grep -o 'attempt=[0-9a-f]*' "$events" | sort -u | wc -l)" -eq 2 ] || { echo "scenario n5a-contract-second-attempt: attempt ids repeat" >&2; exit 1; }

# A missing table is recorded as absent with no rows and an empty copy file.
state_dir="$(fresh_scenario_dir n5a-contract-absent-table)"
mkdir -p "$state_dir/control"
touch "$state_dir/control/absent-table-n5a_beta"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 0 "$status" n5a-contract-absent-table
absent_dir="$(find "$state_dir/contract-snapshots" -mindepth 1 -maxdepth 1 -type d)"
node -e '
  const fs = require("fs");
  const m = JSON.parse(fs.readFileSync(process.argv[1] + "/manifest.json", "utf8"));
  const t = m.tables.find((e) => e.table === "n5a_beta");
  if (!t || t.rows !== 0 || t.absent !== true || fs.statSync(process.argv[1] + "/n5a_beta.copy").size !== 0) {
    console.error("scenario n5a-contract-absent-table: wanted rows 0, absent true and an empty copy file, got " + JSON.stringify(t));
    process.exit(1);
  }
' "$absent_dir"

# Failing gate steps run no migrator and restart the incumbent.
contract_fails n5a-contract-copy-fails fail-copy-n5a_beta "contract snapshot or attempt record failed (no migration ran)"
contract_fails n5a-contract-rename-fails fail-attempt-rename "contract snapshot or attempt record failed (no migration ran)"
contract_fails n5a-contract-line-count-mismatch rows-n5a_alpha "contract snapshot or attempt record failed (no migration ran)"
contract_fails n5a-contract-sha-mismatch tamper-copy "contract snapshot or attempt record failed (no migration ran)"
contract_fails n5a-contract-ledger-row "ledger-row-$contract_version" "already"
contract_fails n5a-contract-schema-contracts-row "schema-contracts-table contract-row-$contract_version" "already"
[ ! -d "$work_dir/scenarios/n5a-contract-copy-fails/contract-attempts" ] || [ -z "$(ls "$work_dir/scenarios/n5a-contract-copy-fails/contract-attempts")" ] \
  || { echo "scenario n5a-contract-copy-fails: an attempt record exists after a failed COPY" >&2; exit 1; }

contract_end

# Without a contract migration the migrator gets no attempt variable.
state_dir="$(fresh_scenario_dir n5a-unmarked-no-attempt)"
set +e
output="$(run_cutover "$state_dir" 2>&1)"
status=$?
set -e
expect_exit 0 "$status" n5a-unmarked-no-attempt
grep -qx 'migrate-run direction=up attempt= passed=no' "$state_dir/control/events.log" \
  || { echo "scenario n5a-unmarked-no-attempt: the migrator got MULTICA_CONTRACT_ATTEMPT" >&2; exit 1; }
[ ! -e "$state_dir/contract-snapshots" ] && [ ! -e "$state_dir/contract-attempts" ] \
  || { echo "scenario n5a-unmarked-no-attempt: contract directories exist" >&2; exit 1; }
echo "n5a contract scenarios passed"

fi

flag_guard_failures=0
for regression in unset-file missing-public-flag; do
  (
    state_dir="$(fresh_scenario_dir "flags-guard-$regression")"
    mkdir -p "$state_dir/control"
    if [ "$regression" = unset-file ]; then
      write_jev_env
      touch "$state_dir/control/flags-mount-backend-blue"
      want="flags continuity: MULTICA_FEATURE_FLAGS_FILE and MULTICA_JEV_FLAGS_FILE are unset"
    else
      printf 'project_workflows_v1:\n  default: true\n' >"$jev_flags"
      write_jev_env "MULTICA_FEATURE_FLAGS_FILE=$jev_flags"
      printf '{"feature_flags":{}}\n' >"$state_dir/control/config-backend-green"
      want="frontend flag mismatch: project_workflows_v1"
    fi
    output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
    expect_exit 1 "$status" "flags-guard-$regression"
    expect_contains "$output" "$want" "flags-guard-$regression"
    [ ! -f "$state_dir/cutover-state.json" ] || exit 1
    [ -f "$state_dir/control/running-backend-blue" ] || exit 1
    [ ! -f "$state_dir/control/running-backend-green" ] || exit 1
    if [ "$regression" = unset-file ] && mutated "$state_dir"; then
      echo "scenario flags-guard-$regression: mutated before refusing" >&2
      exit 1
    fi
    echo "flags-guard-$regression passed"
  ) || flag_guard_failures=$((flag_guard_failures + 1))
done
[ "$flag_guard_failures" -eq 0 ] || exit 1

for setting in MULTICA_FEATURE_FLAGS_FILE MULTICA_JEV_FLAGS_FILE; do
  printf 'project_workflows_v1:\n  default: true\nplugins_v1:\n  default: false\n' >"$jev_flags"
  write_jev_env "$setting=$jev_flags"
  state_dir="$(fresh_scenario_dir "flags-guard-match-$setting")"
  mkdir -p "$state_dir/control"
  config='{"feature_flags":{"project_workflows_v1":true,"plugins_v1":false}}'
  printf '%s\n' "$config" >"$state_dir/control/config-backend-green"
  printf '%s\n' "$config" >"$state_dir/control/config-backend-blue"
  output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
  expect_exit 0 "$status" "flags-guard-match-$setting"
  state_before="$(cat "$state_dir/cutover-state.json")"
  printf '{"feature_flags":{}}\n' >"$state_dir/control/config-backend-blue"
  output="$(run_jev "$state_dir" rollback 2>&1)" && status=0 || status=$?
  expect_exit 1 "$status" "flags-guard-rollback-mismatch-$setting"
  expect_contains "$output" 'frontend flag mismatch: project_workflows_v1' "flags-guard-rollback-mismatch-$setting"
  [ "$(cat "$state_dir/cutover-state.json")" = "$state_before" ] || exit 1
  [ -f "$state_dir/control/running-backend-green" ] || exit 1
  [ ! -f "$state_dir/control/running-backend-blue" ] || exit 1
  [ "$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1])).colour)' "$state_dir/router-state/active.json")" = green ] || exit 1
  write_jev_env
  output="$(run_jev "$state_dir" rollback 2>&1)" && status=0 || status=$?
  expect_exit 1 "$status" "flags-guard-rollback-unset-$setting"
  expect_contains "$output" 'flags continuity:' "flags-guard-rollback-unset-$setting"
  [ "$(cat "$state_dir/cutover-state.json")" = "$state_before" ] || exit 1
  write_jev_env "$setting=$jev_flags"
  printf '%s\n' "$config" >"$state_dir/control/config-backend-blue"
  output="$(run_jev "$state_dir" rollback 2>&1)" && status=0 || status=$?
  expect_exit 0 "$status" "flags-guard-rollback-match-$setting"
  echo "flags-guard-match-and-rollback-$setting passed"
done

for failure in malformed unavailable false; do
  state_dir="$(fresh_scenario_dir "flags-guard-$failure")"
  mkdir -p "$state_dir/control"
  case "$failure" in
    malformed) printf 'invalid-json\n' >"$state_dir/control/config-backend-green" ;;
    unavailable) touch "$state_dir/control/config-unavailable-backend-green" ;;
    false) printf '{"feature_flags":{"project_workflows_v1":false}}\n' >"$state_dir/control/config-backend-green" ;;
  esac
  output="$(run_jev "$state_dir" cutover 2>&1)" && status=0 || status=$?
  expect_exit 1 "$status" "flags-guard-$failure"
  expect_contains "$output" 'frontend flag verification failed' "flags-guard-$failure"
  expect_contains "$output" 'RECOVERED:' "flags-guard-$failure"
  [ ! -f "$state_dir/cutover-state.json" ] || exit 1
  [ -f "$state_dir/control/running-backend-blue" ] || exit 1
  [ ! -f "$state_dir/control/running-backend-green" ] || exit 1
  echo "flags-guard-$failure passed"
done

echo "cutover.sh control-flow fixtures passed"
