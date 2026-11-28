#!/usr/bin/env bash
set -euo pipefail

# Rehearsal of the REAL cutover controller (CHE-7614, N6a).
#
# Runs the unmodified deploy/cd/cutover.sh of --tree against a throwaway A/B
# stack: its own Compose project (multica-rehearsal-<run id>), Postgres volume,
# slot ports, state directories, loopback registry and router container. A
# "previous" backend image (the tree without the contract fixture) is the
# incumbent; a "candidate" image (the tree plus
# deploy/cd/fixtures/contract/*.up.sql) is cut over to. The web image is a
# stand-in (nginx on :3000): the controller only needs it to answer on /.
#
# Builds, images and drills run on the build host only. Never run this on the deploy host.

usage() {
  cat <<'EOF'
usage: rehearse-cutover.sh [--run-id ID] [--tree DIR] [--fault readyz]
                               [--then rollback] [--dry-run]

  --run-id ID       [a-z0-9][a-z0-9-]*, at most 30 chars. Default: r<epoch>-<pid>.
  --tree DIR        Git tree whose cutover.sh and server/migrations are rehearsed.
                    Default: the tree this script lives in.
  --fault readyz    Candidate backend cannot reach its database (drill shim).
  --then rollback   After a completed cutover, run `cutover.sh rollback`.
  --dry-run         Print the plan and exit; no Docker call, no files.

Environment:
  REHEARSE_BASE     Parent of every run directory
                    (default ~/.local/state/multica-rehearsal).
Refuses: any C00_* variable, GHCR_PULL_TOKEN, DATABASE_URL, CUTOVER_DATABASE_URL.
Exit: 0 rehearsal ran (read the evidence block), 2 refused, other = harness failure.
EOF
}

die() { echo "refused: $*" >&2; exit 2; }

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
run_id="r$(date +%s)-$$"
tree="$(cd "$script_dir/../.." && pwd)"
fault=""
then_step=""
dry_run=0

while (($#)); do
  case "$1" in
    --run-id) [ $# -ge 2 ] || die "--run-id needs a value"; run_id=$2; shift 2 ;;
    --tree) [ $# -ge 2 ] || die "--tree needs a value"; tree=$2; shift 2 ;;
    --fault) [ $# -ge 2 ] || die "--fault needs a value"; fault=$2; shift 2 ;;
    --then) [ $# -ge 2 ] || die "--then needs a value"; then_step=$2; shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    --help | -h) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

# --- refusals: before any Docker call and before any file is created ---
for name in $(compgen -e); do
  case "$name" in C00_*) die "$name is set; the rehearsal never reads C00 variables (unset it)" ;; esac
done
[ -z "${GHCR_PULL_TOKEN:-}" ] || die "GHCR_PULL_TOKEN is set; the rehearsal pulls from its own loopback registry only (unset it)"
if [ -n "${CUTOVER_DATABASE_URL:-}${DATABASE_URL:-}" ]; then
  die "a database URL is set; the rehearsal uses its own Postgres service and builds the URL itself (unset DATABASE_URL and CUTOVER_DATABASE_URL)"
fi
[[ "$run_id" =~ ^[a-z0-9][a-z0-9-]{0,29}$ ]] || die "invalid --run-id '$run_id' (want [a-z0-9][a-z0-9-]*, at most 30 chars)"
case "$fault" in "" | readyz) ;; *) die "unknown --fault '$fault' (want readyz)" ;; esac
case "$then_step" in "" | rollback) ;; *) die "unknown --then '$then_step' (want rollback)" ;; esac
[ -z "$fault" ] || [ -z "$then_step" ] || die "--then rollback needs a completed cutover, and --fault readyz never completes one"

base="${REHEARSE_BASE:-$HOME/.local/state/multica-rehearsal}"
run_dir="$base/$run_id"
project="multica-rehearsal-$run_id"
[ ! -e "$run_dir" ] || die "run directory $run_dir already exists"

if [ "$dry_run" = 0 ]; then
  # The router listens on fixed host ports (cutover.sh router_api_port/router_web_port).
  for port in 8081 3000; do
    if ss -ltnH 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]${port}\$"; then
      die "router port $port is busy (another router or rehearsal is running); the controller cannot move it"
    fi
  done
fi

src="$run_dir/tree"
compose_dir="$run_dir/compose"
state_dir="$run_dir/cutover-state"
router_state_dir="$run_dir/router-state"
role_label=org.multica.rehearsal.role
run_label="org.multica.rehearsal.run=$run_id"

if [ "$dry_run" = 1 ]; then
  cat <<EOF
project=$project
run_dir=$run_dir
tree=$tree
compose_dir=$compose_dir
state_dir=$state_dir
router_state_dir=$router_state_dir
router_container=$project-router
registry_container=$project-registry
label=$run_label
label=$role_label=previous
label=$role_label=candidate
EOF
  [ -z "$fault" ] || echo "DRILL_FAULT_READYZ=1"
  [ -z "$then_step" ] || echo "then=$then_step"
  exit 0
fi

# ---------------------------------------------------------------------------
# From here on the run owns Docker artifacts. Cleanup removes only those named
# after this run's project or labelled with its run id.
# ---------------------------------------------------------------------------
df_line() { df -B1 --output=target,used,avail "$base" / | tail -n +2 | tr -s ' ' | paste -sd';'; }

# Every Compose call and the controller run in an ALLOWLISTED environment
# (env -i): nothing else the caller exported (DATABASE_REPLICA_URL, REDIS_URL,
# MULTICA_BACKEND_IMAGE, MULTICA_IMAGE_TAG, ...) reaches Compose or the
# controller, so the stack uses only its own database and images.
iso_env=()
for v in HOME PATH USER LANG TMPDIR DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG XDG_RUNTIME_DIR XDG_CONFIG_HOME; do
  [ -z "${!v:-}" ] || iso_env+=("$v=${!v}")
done
# The run's exact Compose configuration, from the run's own directory: logs and
# teardown must not depend on the caller's working directory or compose files.
rc_compose() {
  (cd "$compose_dir" && env -i "${iso_env[@]}" "COMPOSE_PROJECT_NAME=$project" \
    docker compose -p "$project" -f docker-compose.selfhost.yml -f "$src/deploy/cd/docker-compose.ab.yml" "$@")
}
by_project=(--filter "label=com.docker.compose.project=$project")
# Everything of this run that still exists, by label (independent of any Compose file).
leftovers() {
  docker ps -aq "${by_project[@]}" 2>&1
  docker volume ls -q "${by_project[@]}" 2>&1
  docker network ls -q "${by_project[@]}" 2>&1
  docker ps -aq --filter "label=$run_label" 2>&1
}

cleanup() {
  local rc=$? ref out left have_stack=0
  trap - EXIT
  set +e
  [ -f "$compose_dir/.env" ] && [ -f "$src/deploy/cd/docker-compose.ab.yml" ] && have_stack=1
  if [ "$rc" != 0 ] && [ "$have_stack" = 1 ]; then
    echo "==> harness failed (exit $rc); last log lines of this run's containers"
    rc_compose logs --no-color --tail 25 2>&1 | sed 's/^/  | /'
  fi
  echo "==> cleanup: removing only artifacts of run $run_id"
  if [ "$have_stack" = 1 ]; then
    out="$(rc_compose down -v --remove-orphans 2>&1)" || echo "!! compose down failed: $(echo "$out" | tr '\n' ' ')" >&2
  fi
  docker rm -fv "$project-router" "$project-registry" >/dev/null 2>&1
  while IFS= read -r ref; do
    [ -n "$ref" ] && docker image rm "$ref" >/dev/null 2>&1
  done < <(docker image ls --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -F "/$project/")
  # Verify by label: remove what is left of this run (only this run), then
  # report anything that still exists.
  { docker ps -aq "${by_project[@]}"; docker ps -aq --filter "label=$run_label"; } 2>/dev/null | xargs -r docker rm -fv >/dev/null 2>&1
  docker volume ls -q "${by_project[@]}" 2>/dev/null | xargs -r docker volume rm >/dev/null 2>&1
  docker network ls -q "${by_project[@]}" 2>/dev/null | xargs -r docker network rm >/dev/null 2>&1
  left="$(leftovers | tr '\n' ' ')"
  if [ -n "${left//[[:space:]]/}" ]; then
    echo "!! cleanup incomplete: artifacts of run $run_id survive: $left" >&2
    [ "$rc" != 0 ] || rc=3
  fi
  rm -rf "$run_dir"
  echo "df -B1 after: $(df_line)"
  exit "$rc"
}

mkdir -p "$base"
exec 8>"$base/.lock"
flock -n 8 || die "another rehearsal holds $base/.lock; the router ports are shared"
mkdir "$run_dir"
trap cleanup EXIT
echo "df -B1 before: $(df_line)"

mkdir -p "$compose_dir" "$state_dir" "$router_state_dir"
secret() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
jwt_secret="$(secret)"
pg_password="$(secret)"

# Five free loopback ports: registry + the four slot ports.
read -r registry_port backend_blue backend_green frontend_blue frontend_green < <(
  node -e '
    const net = require("node:net");
    const ports = [];
    const next = () => {
      if (ports.length === 5) { console.log(ports.join(" ")); return; }
      const s = net.createServer();
      s.listen(0, "127.0.0.1", () => { ports.push(s.address().port); s.close(next); });
    };
    next();
  '
)
registry="127.0.0.1:$registry_port"

# --- tree copy: the controller's own root_dir, so it reads this copy's
# server/migrations and compose files ---
mkdir "$src"
git -C "$tree" ls-files -z --cached --others --exclude-standard | tar -C "$tree" --null -T - -cf - | tar -C "$src" -xf -
candidate_sha="$(git -C "$tree" rev-parse HEAD)"
previous_sha="$(printf 'previous-%s' "$run_id" | sha1sum | cut -c1-40)"
fixture_dir="$tree/deploy/cd/fixtures/contract"
fixture_version="$(basename "$(ls "$fixture_dir"/*.up.sql)" .up.sql)"

backend_repo="$registry/$project/backend"
web_repo="$registry/$project/web"

build_backend() { # <role> <sha>
  docker build -q -f "$src/Dockerfile" --build-arg "COMMIT=$2" \
    --label "$run_label" --label "$role_label=$1" \
    -t "$backend_repo:sha-$2" "$src" >/dev/null
}
build_web() { # <role> <sha>
  printf 'FROM nginx:alpine\nRUN sed -i "s/listen  *80;/listen 3000;/" /etc/nginx/conf.d/default.conf\n' |
    docker build -q -f - --label "$run_label" --label "$role_label=$1" -t "$web_repo:sha-$2" "$src/deploy" >/dev/null
}

echo "==> building images (previous = tree without the contract fixture)"
build_backend previous "$previous_sha"
build_web previous "$previous_sha"
cp "$fixture_dir"/*.sql "$src/server/migrations/"
build_backend candidate "$candidate_sha"
build_web candidate "$candidate_sha"

echo "==> starting loopback registry on $registry and pushing"
docker run -d --name "$project-registry" --label "$run_label" -p "127.0.0.1:$registry_port:5000" registry:2 >/dev/null
for _ in $(seq 30); do curl -fsS "http://$registry/v2/" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "http://$registry/v2/" >/dev/null
for ref in "$backend_repo:sha-$previous_sha" "$web_repo:sha-$previous_sha" "$backend_repo:sha-$candidate_sha" "$web_repo:sha-$candidate_sha"; do
  docker push -q "$ref" >/dev/null
done
digest_of() { docker image inspect --format '{{index .RepoDigests 0}}' "$1" | sed -E 's#^.*@##'; }
prev_backend_digest="$(digest_of "$backend_repo:sha-$previous_sha")"
prev_web_digest="$(digest_of "$web_repo:sha-$previous_sha")"
cand_backend_digest="$(digest_of "$backend_repo:sha-$candidate_sha")"
cand_web_digest="$(digest_of "$web_repo:sha-$candidate_sha")"

# --- compose dir, as the controller expects to find it on a host ---
node "$src/deploy/cd/drift-receipt.mjs" sync --source-root "$src" --compose-dir "$compose_dir"
cat >"$compose_dir/.env" <<EOF
JWT_SECRET=$jwt_secret
MULTICA_SECRETS_KEY=$(head -c 32 /dev/urandom | base64)
POSTGRES_PASSWORD=$pg_password
BACKEND_BLUE_PORT=$backend_blue
BACKEND_GREEN_PORT=$backend_green
FRONTEND_BLUE_PORT=$frontend_blue
FRONTEND_GREEN_PORT=$frontend_green
MULTICA_BACKEND_IMAGE=$backend_repo
MULTICA_WEB_IMAGE=$web_repo
MULTICA_IMAGE_TAG=sha-$previous_sha
EOF
chmod 600 "$compose_dir/.env"

slot_ports=("BACKEND_BLUE_PORT=$backend_blue" "BACKEND_GREEN_PORT=$backend_green" "FRONTEND_BLUE_PORT=$frontend_blue" "FRONTEND_GREEN_PORT=$frontend_green")
# The generated .env pins the images and ports; the isolated environment (above)
# keeps the caller's exports from overriding them.

echo "==> bootstrapping the incumbent (blue, previous image) and the router"
rc_compose up -d --wait postgres
rc_compose run --rm --no-deps --entrypoint ./migrate backend-blue up
MULTICA_SKIP_MIGRATIONS=1 rc_compose up -d --no-deps backend-blue frontend-blue
for _ in $(seq 60); do curl -fsS "http://127.0.0.1:$backend_blue/readyz" >/dev/null 2>&1 && break; sleep 2; done
curl -fsS "http://127.0.0.1:$backend_blue/readyz" >/dev/null
router_image="${ROUTER_NGINX_IMAGE:-nginx:alpine}"
env -i "${iso_env[@]}" "${slot_ports[@]}" ROUTER_NGINX_IMAGE="$router_image" ROUTER_RELOAD_CMD=true bash "$src/deploy/cd/router.sh" select --colour blue --state-dir "$router_state_dir" >/dev/null
docker run -d --name "$project-router" --label "$run_label" --network host \
  -v "$src/deploy/cd/router/nginx.conf:/etc/nginx/nginx.conf:ro" -v "$router_state_dir:/etc/nginx/router-state:ro" \
  "$router_image" >/dev/null

# --- manifest, baseline tuple, observed state and packet for the candidate ---
rc_compose exec -T postgres psql -U multica -d multica -tA -c 'SELECT version FROM schema_migrations ORDER BY version COLLATE "C"' >"$run_dir/ledger.txt"
node - "$run_dir" "$src" "$candidate_sha" "$backend_repo" "$web_repo" "$prev_backend_digest" "$prev_web_digest" "$cand_backend_digest" "$cand_web_digest" <<'NODE'
const fs = require("node:fs");
const crypto = require("node:crypto");
const cp = require("node:child_process");
const [runDir, src, sha, backendRepo, webRepo, prevB, prevW, candB, candW] = process.argv.slice(2);
const versions = fs.readFileSync(`${runDir}/ledger.txt`, "utf8").split("\n").filter(Boolean);
const sha256 = (b) => `sha256:${crypto.createHash("sha256").update(b).digest("hex")}`;
const baseline = {
  migration_ledger: { row_count: versions.length, ordered_sha256: sha256(`${versions.join("\n")}\n`), latest: { version: versions.at(-1) } },
  images: { backend: { digest: prevB }, web: { digest: prevW } },
};
const baselineBytes = `${JSON.stringify(baseline, null, 2)}\n`;
fs.writeFileSync(`${runDir}/baseline-tuple.json`, baselineBytes);
fs.writeFileSync(`${runDir}/observed-state.json`, `${JSON.stringify({
  schema_version: 1,
  ledger: { status: "complete", versions },
  indexes: { status: "complete", invalid: [] },
  hooks: { status: "complete", observations: [] },
}, null, 2)}\n`);
// release-manifest.mjs insists on ghcr.io images, so the manifest is written directly.
const inventory = cp.execFileSync("node", [`${src}/deploy/cd/migration-inventory.mjs`, `${src}/server/migrations`], { encoding: "utf8" }).trim();
fs.writeFileSync(`${runDir}/manifest.json`, `${JSON.stringify({
  schema_version: 1,
  kind: "release-candidate",
  repository: "cheese-work/multica",
  source_sha: sha,
  architecture: "linux/amd64",
  configuration_sha256: sha256("rehearsal"),
  migration_inventory_sha256: inventory,
  fork_changelog_sha256: "absent",
  baseline_tuple_sha256: sha256(baselineBytes),
  images: { backend: `${backendRepo}@${candB}`, web: `${webRepo}@${candW}` },
  image_digests: { backend: candB, web: candW },
}, null, 2)}\n`);
NODE
node "$src/deploy/cd/release-packet.mjs" create --manifest "$run_dir/manifest.json" --baseline-tuple "$run_dir/baseline-tuple.json" \
  --observed-state "$run_dir/observed-state.json" --migrations-dir "$src/server/migrations" --output "$run_dir/packet.json"

# --- the rehearsal: the real controller, nothing stubbed ---
controller_env=(
  "COMPOSE_PROJECT_NAME=$project"
  "CUTOVER_DATABASE_URL=postgres://multica:$pg_password@127.0.0.1:5432/multica?sslmode=disable"
  "ROUTER_STATE_DIR=$router_state_dir"
  "ROUTER_CONTAINER_NAME=$project-router"
  "ROUTER_NGINX_IMAGE=$router_image"
  "CUTOVER_LOCK_WAIT_SECONDS=30"
)
controller_path="$PATH"
if [ -n "$fault" ]; then
  controller_env+=("DRILL_FAULT_READYZ=1")
  controller_path="$src/deploy/cd/drill:$PATH"
fi
run_controller() {
  local rc=0
  env -i "${iso_env[@]}" "${slot_ports[@]}" "${controller_env[@]}" PATH="$controller_path" bash "$src/deploy/cd/cutover.sh" "$@" || rc=$?
  return "$rc"
}

echo "==> cutover.sh cutover${fault:+ (fault: $fault)}"
cutover_rc=0
run_controller cutover --manifest "$run_dir/manifest.json" --packet "$run_dir/packet.json" \
  --compose-dir "$compose_dir" --state-dir "$state_dir" || cutover_rc=$?

rollback_rc=""
if [ "$then_step" = rollback ]; then
  if [ "$cutover_rc" = 0 ]; then
    echo "==> cutover.sh rollback"
    rollback_rc=0
    run_controller rollback --compose-dir "$compose_dir" --state-dir "$state_dir" || rollback_rc=$?
  else
    echo "==> skipping rollback: the cutover did not complete (exit $cutover_rc)"
  fi
fi

# --- evidence: what the controller left behind, read back from the stack ---
final_ledger="$(rc_compose exec -T postgres psql -U multica -d multica -tA -c 'SELECT max(version) FROM schema_migrations' 2>/dev/null | tr -d '[:space:]' || true)"
running="$(rc_compose ps --status running --format '{{.Service}}' 2>/dev/null | grep -E '^(backend|frontend)-' | sort | paste -sd, || true)"
router_colour="$(node -e 'try{console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).colour)}catch{console.log("")}' "$router_state_dir/active.json")"
state_colour="$(node -e 'try{console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).active_colour)}catch{console.log("")}' "$state_dir/cutover-state.json")"
echo "==> evidence"
echo "evidence.run_id=$run_id"
echo "evidence.controller_tree_head=$candidate_sha"
echo "evidence.fault=${fault:-none}"
echo "evidence.cutover_exit=$cutover_rc"
echo "evidence.rollback_exit=${rollback_rc:-not-run}"
echo "evidence.ledger_head=$final_ledger"
echo "evidence.contract_row_present=$([ "$final_ledger" = "$fixture_version" ] && echo yes || echo no)"
echo "evidence.running_services=$running"
echo "evidence.router_colour=$router_colour"
echo "evidence.state_active_colour=${state_colour:-none}"
[ -f "$state_dir/outage-alert.json" ] && echo "evidence.outage_alert=$(tr -d '\n' <"$state_dir/outage-alert.json")"
if [ "$cutover_rc" != 0 ] && [ "$final_ledger" = "$fixture_version" ] && [[ ",$running," == *",backend-blue,"* ]]; then
  echo "observed: the incumbent restarted with the contract row present (ledger $final_ledger)"
fi
exit 0
