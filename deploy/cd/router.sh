#!/usr/bin/env bash
set -euo pipefail

# Local A/B router controller (CHE-397 unit 2). Runs ON C00, invoked by
# cutover.sh under the same deploy lock deploy.sh already uses — this script
# never acquires its own lock, and never runs outside one.
#
# The router is a single small Nginx process, bound to the private
# 127.0.0.1:8081 (API) and 127.0.0.1:3000 (frontend) upstream addresses that
# Tailscale Serve's existing :19444/:19443 already forward to (see the
# accepted architecture's "Routing and TLS switch"). This script never
# touches Tailscale Serve, TLS, or certificates — only which colour those
# two stable addresses forward to.
#
# ## "Atomic" = generation selection, not socket transfer
#
# Each call to `select` renders a COMPLETE, fully independent config file
# for both listeners into deploy/cd/router/generations/<n>.conf, validates
# it with `nginx -t` BEFORE it is live, then does the one operation that is
# actually atomic on a POSIX filesystem: `ln -sfn` repoints the
# `router/active.conf` symlink from the old generation file to the new one
# in a single rename syscall. `nginx -s reload` (SIGHUP) then has the
# already-active master process re-read `active.conf` (which the running
# config's own `include` line names) and spawn new workers on the new
# config; old workers finish in-flight requests against the OLD config and
# exit once idle or once the drain timeout below elapses — no listening
# socket is ever closed and re-opened, and no request is ever routed by a
# half-written config, because the symlink swap and the validation that
# gates it both happen before reload is ever signalled.
#
# The old generation file is never deleted by `select` — `retained-count`
# below prunes anything past the newest N, so a manual `revert` always has
# somewhere to point back to without regenerating a config from memory.

usage() {
  cat <<'EOF'
usage: router.sh <command> [options]

Commands:
  select --colour blue|green --state-dir PATH [--retained-count N]
      Render, validate and activate a complete router generation pointing
      both listeners at the given colour's private ports. Reloads Nginx
      under the caller's lock. Retains the previous N generations (default
      5) for `revert`.

  revert --state-dir PATH
      Re-activate the generation active.conf pointed at immediately BEFORE
      the current one, and reload. Fails if there is no prior generation
      recorded (first-ever `select` in this state dir).

  probe --state-dir PATH
      Print the colour the router currently forwards to (from the active
      generation's recorded metadata) and exit 0 if that colour's own
      backend/frontend containers answer locally. Never touches Nginx.

  validate --colour blue|green --state-dir PATH
      Render and `nginx -t` a candidate generation without activating it.
      Used to prove a colour's config is well-formed before a cutover
      commits to it.

State directory layout (under --state-dir, default deploy/cd/router/state):
  generations/<timestamp>-<colour>.<unique>.conf   one file per rendered generation
  generations/<timestamp>-<colour>.<unique>.json   colour + port metadata for that generation
  active.conf -> generations/<...>.conf   the symlink Nginx's own config includes
  active.json -> generations/<...>.json
  previous.sha256                         retained config + metadata checksums for revert
  history.log                              append-only "<timestamp> <colour> <path>" audit trail
  nginx.pid                                the running router's PID (from `docker inspect`/`pidof` at start time)
EOF
}

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
router_dir="$root_dir/deploy/cd/router"
template="$router_dir/nginx.conf.template"

command=${1:-}
shift || true

if [ "$command" = "--help" ] || [ "$command" = "-h" ] || [ -z "$command" ]; then
  usage
  exit 0
fi

colour=""
state_dir="$router_dir/state"
retained_count=5

while (($#)); do
  case "$1" in
    --colour) colour=${2:?}; shift 2 ;;
    --state-dir) state_dir=${2:?}; shift 2 ;;
    --retained-count) retained_count=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ ! -f "$template" ]; then
  echo "router config template missing: $template" >&2
  exit 1
fi

generations_dir="$state_dir/generations"
mkdir -p "$generations_dir"

# port_for reads the port for a colour/role from the caller's environment
# and REFUSES when it is unset — no fallback. A literal default here is a
# second interpretation of docker-compose.ab.yml that silently disagrees
# with Compose whenever .env overrides a port but the caller never exported
# it (CHE-773). cutover.sh passes the ports Compose actually renders
# (compose_rendered_port); a manual caller must pass them too.
port_for() {
  local role=$1 colour=$2 var
  case "${role}_${colour}" in
    backend_blue) var=BACKEND_BLUE_PORT ;;
    backend_green) var=BACKEND_GREEN_PORT ;;
    frontend_blue) var=FRONTEND_BLUE_PORT ;;
    frontend_green) var=FRONTEND_GREEN_PORT ;;
    *) echo "unknown role/colour: $role/$colour" >&2; return 1 ;;
  esac
  if ! [[ "${!var:-}" =~ ^[0-9]+$ ]]; then
    echo "!! $var is not set to a port; router.sh never guesses — pass the port Compose actually publishes (docker compose ... port ${role}-${colour})" >&2
    return 1
  fi
  echo "${!var}"
}

# render writes a complete, self-contained config for the given colour to
# stdout — never a partial fragment that depends on a previously active
# file, so validating or activating one generation never depends on the
# state of another.
render() {
  local colour=$1
  local backend_port frontend_port
  backend_port="$(port_for backend "$colour")"
  frontend_port="$(port_for frontend "$colour")"
  sed \
    -e "s/ACTIVE_BACKEND_UPSTREAM/127.0.0.1:${backend_port}/" \
    -e "s/ACTIVE_FRONTEND_UPSTREAM/127.0.0.1:${frontend_port}/" \
    "$template"
}

# validate_config runs `nginx -t` against a candidate file without ever
# starting or reloading a real listener — a syntactically invalid or
# semantically broken (e.g. bad upstream syntax) config is caught here,
# before `select` ever repoints the active symlink. Uses the same
# nginx:1.27-alpine image `test-router.sh` pins, so validation behavior is
# identical between a developer's machine and CI regardless of whether
# nginx is installed on the host.
validate_config() {
  local candidate=$1
  local image="${ROUTER_NGINX_IMAGE:-nginx:1.27-alpine}"
  if ! docker run --rm \
    -v "$candidate:/etc/nginx/nginx.conf:ro" \
    "$image" nginx -t 2>&1; then
    return 1
  fi
}

# reload_router signals the already-running router container to re-read
# active.conf via SIGHUP (`nginx -s reload`), which spawns new worker
# processes against the new config and lets old workers drain in-flight
# connections rather than dropping them. It never starts, stops, or
# recreates the router container itself — `select`/`revert` only ever
# change WHICH file the running process reads; bringing the router
# container up in the first place is deploy/cd/router/docker-compose.router.yml's
# job (see that file's own comment), run once per host, not per cutover.
#
# ROUTER_CONTAINER_NAME defaults to the name docker-compose.router.yml gives
# the service; a test harness overrides it (or ROUTER_RELOAD_CMD directly)
# to point at a scripted fake instead of a real Docker daemon.
reload_router() {
  local state_dir=$1
  if [ -n "${ROUTER_RELOAD_CMD:-}" ]; then
    # A test harness's override is arbitrary shell, which may itself invoke
    # `exit` (a builtin `eval` cannot contain — it would terminate this
    # whole script, not just the function). Run it in a subshell so its
    # exit status is observable here without ending router.sh outright.
    if ! (eval "$ROUTER_RELOAD_CMD"); then
      echo "!! router reload failed (ROUTER_RELOAD_CMD override) — active.conf was updated but the running process may still be serving the previous generation" >&2
      return 1
    fi
    return 0
  fi
  local container="${ROUTER_CONTAINER_NAME:-multica-ab-router}"
  if ! docker exec "$container" nginx -s reload 2>&1; then
    echo "!! router reload failed against container '$container' — active.conf was updated but the running process may still be serving the previous generation" >&2
    return 1
  fi
}

record_history() {
  local colour=$1 path=$2
  printf '%s %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$colour" "$path" >>"$state_dir/history.log"
}

check_generation() {
  local relative=$1
  if ! [[ "$relative" =~ ^generations/[[:alnum:]_.-]+\.conf$ ]] ||
    [ ! -f "$state_dir/$relative" ] || [ -L "$state_dir/$relative" ] ||
    [ ! -f "$state_dir/${relative%.conf}.json" ] || [ -L "$state_dir/${relative%.conf}.json" ]; then
    echo "!! generation target or metadata is missing or unsafe" >&2
    return 1
  fi
  node - "$state_dir/$relative" "$state_dir/${relative%.conf}.json" <<'NODE'
const filesystem = require('fs');
const config = filesystem.readFileSync(process.argv[2], 'utf8');
const metadata = JSON.parse(filesystem.readFileSync(process.argv[3], 'utf8'));
const ports = [metadata.backend_port, metadata.frontend_port];
const expected = ports.map(port => `http://127.0.0.1:${port}`);
const actual = [...config.matchAll(/^\s*proxy_pass\s+([^;]+);/gm)].map(match => match[1]);
if (!['blue', 'green'].includes(metadata.colour) || typeof metadata.generated_at !== 'string' ||
    !ports.every(port => Number.isInteger(port) && port > 0 && port <= 65535) ||
    !expected.every(upstream => actual.includes(upstream)) ||
    actual.some(upstream => !expected.includes(upstream))) {
  throw new Error('generation bytes and metadata disagree');
}
NODE
}

check_retained_generation() {
  check_generation "$previous_relative" &&
    (cd "$state_dir" && sha256sum "$previous_relative" "${previous_relative%.conf}.json" | cmp -s previous.sha256 -) || {
      echo "!! retained generation byte/metadata integrity check failed" >&2
      return 1
    }
}

prune_generations() {
  # Keep the newest $retained_count *.conf/*.json pairs; anything older is
  # removed. `active.conf`/`active.json` are symlinks, not real files under
  # generations/, so they are never candidates for pruning themselves —
  # only what they point at can be, and only once it is no longer the
  # newest N.
  local keep=$retained_count
  local active_target previous_target
  active_target="$(readlink "$state_dir/active.conf")"
  previous_target="$(cat "$state_dir/previous.conf.path" 2>/dev/null || true)"
  local -a files
  mapfile -t files < <(find "$generations_dir" -maxdepth 1 -name '*.conf' -printf '%T@ %p\n' | sort -rn | awk '{print $2}')
  local i=0
  for f in "${files[@]}"; do
    i=$((i + 1))
    if [ "$f" = "$state_dir/$active_target" ] || [ "$f" = "$state_dir/$previous_target" ]; then
      continue
    fi
    if [ "$i" -gt "$keep" ]; then
      rm -f "$f" "${f%.conf}.json"
    fi
  done
}

case "$command" in
  select)
    if [ -z "$colour" ]; then
      echo "select requires --colour blue|green" >&2
      exit 2
    fi
    case "$colour" in blue | green) ;; *) echo "invalid --colour: $colour (want blue or green)" >&2; exit 2 ;; esac

    previous_target=""
    previous_checksums=""
    if [ -L "$state_dir/active.conf" ]; then
      previous_target="$(readlink "$state_dir/active.conf")"
      check_generation "$previous_target"
      if [ "$(readlink "$state_dir/active.json")" != "${previous_target%.conf}.json" ]; then
        echo "!! active config and metadata targets disagree" >&2
        exit 1
      fi
      previous_checksums="$(cd "$state_dir" && sha256sum "$previous_target" "${previous_target%.conf}.json")"
    elif [ -e "$state_dir/active.conf" ]; then
      echo "!! active config is not a generation symlink" >&2
      exit 1
    fi

    ts="$(date -u +%Y%m%dT%H%M%SZ)"
    candidate_conf="$(mktemp "$generations_dir/${ts}-${colour}.XXXXXX.conf")"
    candidate_json="${candidate_conf%.conf}.json"
    trap 'rm -f "$candidate_conf"' EXIT

    render "$colour" >"$candidate_conf"
    if ! validate_config "$candidate_conf"; then
      echo "!! generated config for colour=$colour failed nginx -t; refusing to activate" >&2
      rm -f "$candidate_conf"
      exit 1
    fi

    (set -o noclobber; cat >"$candidate_json" <<JSON
{"colour": "$colour", "generated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)", "backend_port": $(port_for backend "$colour"), "frontend_port": $(port_for frontend "$colour")}
JSON
    )
    trap 'rm -f "$candidate_conf" "$candidate_json"' EXIT
    check_generation "generations/$(basename "$candidate_conf")"
    chmod 0444 "$candidate_conf" "$candidate_json"

    # Record what was active BEFORE this swap so `revert` has an exact
    # target — reading it off the (about-to-be-replaced) active.conf
    # symlink itself, not off history.log, so a `revert` is correct even if
    # history.log was pruned or lost.
    #
    # Plain `readlink` (not `-f`/`-e`), and stored as the RAW relative
    # target ("generations/<file>") — matching the relative form `ln -sfn`
    # now writes below. Resolving to an absolute path here (readlink -f)
    # would reintroduce the same host-path-dangles-in-the-container defect
    # this fix addresses, just one step removed: `revert` would read an
    # absolute path back out of previous.conf.path and hand it straight to
    # `ln -sfn` again.

    # The symlink swap itself: `ln -sfn` unlinks the old symlink and creates
    # the new one via `rename(2)`, which POSIX guarantees is atomic — a
    # concurrent reader (this script's own `probe`, or a human `readlink`)
    # never observes a half-updated or missing symlink, only the old target
    # or the new one.
    #
    # The link target is RELATIVE (generations/<file>, not the absolute
    # $candidate_conf) — the router container bind-mounts this state
    # directory read-only at a different path than its host location
    # (docker-compose.router.yml mounts ./state at
    # /etc/nginx/router-state), so an absolute host path baked into the
    # symlink dangles inside the container: `nginx -t` inside the container
    # reproduced this exactly as `open() "/etc/nginx/router-state/active.conf"
    # failed (2: No such file or directory)` when the link target was the
    # absolute host path. A relative target resolves correctly under
    # whatever directory the symlink itself lives in, host or container
    # alike.
    trap - EXIT
    ln -sfn "generations/$(basename "$candidate_conf")" "$state_dir/active.conf"
    ln -sfn "generations/$(basename "$candidate_json")" "$state_dir/active.json"
    if [ -n "$previous_target" ]; then
      printf '%s\n' "$previous_target" >"$state_dir/previous.conf.path"
      printf '%s\n' "$previous_checksums" >"$state_dir/previous.sha256"
    fi

    reload_router "$state_dir"
    record_history "$colour" "$candidate_conf"
    prune_generations
    echo "==> router generation activated: colour=$colour config=$candidate_conf"
    ;;

  revert)
    if [ ! -f "$state_dir/previous.conf.path" ]; then
      echo "no previous generation recorded in $state_dir; cannot revert" >&2
      exit 1
    fi
    # previous_relative is the raw relative target ("generations/<file>"),
    # as written by `select` above. previous_conf resolves it against
    # $state_dir for THIS script's own filesystem checks/validation (which
    # run on the host, not inside the container); the symlink itself is
    # still written with the relative form so it resolves correctly under
    # both the host state dir and the container's differently-pathed mount.
    previous_relative="$(cat "$state_dir/previous.conf.path")"
    previous_conf="$state_dir/$previous_relative"
    check_retained_generation
    if ! validate_config "$previous_conf"; then
      echo "!! previous generation failed nginx -t on revert; refusing to activate a config that is now invalid" >&2
      exit 1
    fi
    previous_relative_json="${previous_relative%.conf}.json"
    previous_json="$state_dir/$previous_relative_json"
    ln -sfn "$previous_relative" "$state_dir/active.conf"
    [ -f "$previous_json" ] && ln -sfn "$previous_relative_json" "$state_dir/active.json"
    reload_router "$state_dir"
    if [ "$(readlink "$state_dir/active.conf")" != "$previous_relative" ] ||
      [ "$(readlink "$state_dir/active.json")" != "$previous_relative_json" ]; then
      echo "!! restored generation targets changed during reload" >&2
      exit 1
    fi
    check_retained_generation
    colour_reverted="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).colour)' "$previous_json")"
    record_history "$colour_reverted" "$previous_conf"
    echo "==> router reverted to previous generation: config=$previous_conf"
    ;;

  probe)
    if [ ! -L "$state_dir/active.json" ]; then
      echo "no active router generation in $state_dir" >&2
      exit 1
    fi
    active_colour="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).colour)' "$state_dir/active.json")"
    # The port the active generation actually forwards to, not a re-derived one.
    backend_port="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).backend_port)' "$state_dir/active.json")"
    if curl --fail --silent --show-error "http://127.0.0.1:${backend_port}/readyz" >/dev/null 2>&1; then
      echo "$active_colour"
      exit 0
    fi
    echo "$active_colour"
    exit 1
    ;;

  validate)
    if [ -z "$colour" ]; then
      echo "validate requires --colour blue|green" >&2
      exit 2
    fi
    tmp_conf="$(mktemp)"
    trap 'rm -f "$tmp_conf"' EXIT
    render "$colour" >"$tmp_conf"
    validate_config "$tmp_conf"
    ;;

  *)
    echo "unknown command: $command" >&2
    usage >&2
    exit 2
    ;;
esac
