#!/usr/bin/env bash
# Installs the native C00 health observer (CHE-5175): a systemd timer that runs
# health-window.mjs tick every 5 s. The tick does nothing until a deploy's readback
# arms an observation, then it samples each 10 s slot of the 15-minute window into
# <STATE_DIR>/health-observer. Run once on C00, as root. This is C00 configuration:
# install only after the owner approves.
# usage: install-health-observer.sh STATE_DIR RUN_USER
# STATE_DIR is C00_STATE_DIR (absolute); RUN_USER is the user the CD runner runs as,
# so the journal is written by the same user that arms and verifies it.
# SYSTEMD_UNIT_DIR (default /etc/systemd/system) is the unit directory.
set -euo pipefail

state_dir=${1:?usage: install-health-observer.sh STATE_DIR RUN_USER}
run_user=${2:?usage: install-health-observer.sh STATE_DIR RUN_USER}
[[ "$state_dir" =~ ^/[A-Za-z0-9._/-]+$ ]] || { echo "::error::STATE_DIR must be an absolute path of letters, digits, dot, dash, underscore and slash" >&2; exit 2; }
[[ "$run_user" =~ ^[a-z_][a-z0-9_-]*$ ]] || { echo "::error::RUN_USER is not a valid user name" >&2; exit 2; }
state_dir=${state_dir%/}
node_bin="$(command -v node)" || { echo "::error::node is not installed" >&2; exit 1; }
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
unit_dir=${SYSTEMD_UNIT_DIR:-/etc/systemd/system}

for unit in multica-health-observer.service multica-health-observer.timer; do
  sed -e "s|@STATE_DIR@|${state_dir}|g" -e "s|@USER@|${run_user}|g" -e "s|@NODE@|${node_bin}|g" \
    "$here/observer/$unit" > "$unit_dir/$unit.new"
  mv "$unit_dir/$unit.new" "$unit_dir/$unit"
done
systemctl daemon-reload
systemctl enable --now multica-health-observer.timer
echo "health observer timer installed for ${state_dir}/health-observer (user ${run_user})."
