#!/usr/bin/env bash
set -euo pipefail

# Resolves the A/B router's actual state directory FROM THE RUNNING
# CONTAINER (CHE-773 follow-up), instead of assuming it lives at a fixed
# path relative to the Compose stack's own directory.
#
# `router/docker-compose.router.yml` is brought up once, by hand, as a
# C00-side adoption step (see deploy/cd/README.md, "What this unit does NOT
# cover") — its own compose file, and therefore the host directory its
# `./state:/etc/nginx/router-state:ro` bind mount resolves from, is whoever
# ran that compose file's choice, not necessarily $C00_COMPOSE_DIR. CD run
# 36083685740 attempt 2 assumed they were the same directory
# ($C00_COMPOSE_DIR/deploy/cd/router/state) and got a directory that does
# not exist, even though the router had a healthy, already-adopted
# active.json at its real mount (/home/congvc/.multica/ab/deploy/cd/router/state
# vs. the assumed /home/congvc/.multica/server/deploy/cd/router/state).
#
# `docker inspect` on the container itself is the one source of truth for
# where its bind mount actually resolves on the host — this script never
# guesses or falls back to a derived path.
usage() {
  echo "usage: resolve-router-state-dir.sh [--container NAME]" >&2
}

container="${ROUTER_CONTAINER_NAME:-multica-ab-router}"
while (($#)); do
  case "$1" in
    --container) container=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

mount_source="$(docker inspect --format '{{ range .Mounts }}{{ if eq .Destination "/etc/nginx/router-state" }}{{ .Source }}{{ end }}{{ end }}' "$container" 2>/dev/null || true)"

if [ -z "$mount_source" ]; then
  echo "!! could not resolve /etc/nginx/router-state bind-mount source for container '$container' — is it running, and does it still bind-mount router state at that path?" >&2
  exit 1
fi

if [ ! -d "$mount_source" ]; then
  echo "!! router container '$container' reports its state bind-mount source as '$mount_source', but that directory does not exist on this host" >&2
  exit 1
fi

printf '%s\n' "$mount_source"
