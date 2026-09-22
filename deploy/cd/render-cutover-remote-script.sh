#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: render-cutover-remote-script.sh --router-state-dir PATH -- COMMAND [ARG ...]" >&2
}

router_state_dir=""
while (($#)); do
  case "$1" in
    --router-state-dir) router_state_dir=${2:?}; shift 2 ;;
    --) shift; break ;;
    *) usage; exit 2 ;;
  esac
done
[ -n "$router_state_dir" ] || { usage; exit 2; }
(($#)) || { usage; exit 2; }

printf 'set -euo pipefail\n'
printf 'set -a\n'
printf 'GHCR_PULL_TOKEN=%q\n' "${GHCR_PULL_TOKEN:-}"
printf 'set +a\n'
printf 'export ROUTER_STATE_DIR=%q\n' "$router_state_dir"
printf 'exec'
printf ' %q' "$@"
printf '\n'
