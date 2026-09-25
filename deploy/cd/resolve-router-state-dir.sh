#!/usr/bin/env bash
set -euo pipefail

# Prints the host directory the RUNNING router bind-mounts as its state dir
# (CHE-702). Runs ON C00, read-only. The router may have been brought up
# from a different checkout than C00_COMPOSE_DIR, so the controller and the
# health check must use the directory nginx actually reads, not one derived
# from the Compose path. Refuses (exit 1) when the container is missing,
# has no bind mount at the target, has more than one, or the directory has
# no readable active.json.

container="${ROUTER_CONTAINER_NAME:-multica-ab-router}"
target=/etc/nginx/router-state

mounts="$(docker inspect "$container" \
  --format '{{range .Mounts}}{{if eq .Destination "'"$target"'"}}{{.Type}} {{.Source}}{{println}}{{end}}{{end}}' 2>/dev/null)" || {
  echo "!! router container '$container' not found; cannot resolve its state directory" >&2
  exit 1
}
mounts="$(printf '%s\n' "$mounts" | sed '/^$/d')"
count="$(printf '%s' "$mounts" | grep -c . || true)"
if [ "$count" -ne 1 ]; then
  echo "!! router container '$container' has $count mounts at $target; want exactly one" >&2
  exit 1
fi
type="${mounts%% *}" source="${mounts#* }"
if [ "$type" != bind ] || [ -z "$source" ]; then
  echo "!! router state mount at $target is '$type', not a host bind mount" >&2
  exit 1
fi
if [ ! -r "$source/active.json" ]; then
  echo "!! router state dir $source (mounted at $target) has no readable active.json" >&2
  exit 1
fi
printf '%s\n' "$source"
