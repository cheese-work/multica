#!/usr/bin/env bash
# Sourced by launch.sh, cleanup.sh and drive.sh. Expects $root (the checkout).
# Ownership model: launch.sh claims the checkout only when no environment is
# registered for it, and records a run id plus the environment it created in an
# ownership record outside the checkout. cleanup.sh acts only on an environment
# that matches that record, and stops only processes that carry the run id.
VM_ENVS="${VERIFY_MULTICA_ENVS:-$HOME/.multica/dev/envs}"
VM_STATE="${VERIFY_MULTICA_STATE:-$HOME/.multica/verify-multica}"

vm_marker() { printf '%s/%s.run' "$VM_STATE" "$(printf '%s' "$root" | sha256sum | cut -c1-16)"; }

# vm_value <file> <KEY>: a KEY=value line of a manifest or record, read in a subshell.
vm_value() { ( set +u; . "$1" 2>/dev/null; eval "printf '%s' \"\${$2:-}\"" ); }

# The manifest of the environment registered for this checkout, or nothing.
vm_manifest_for_root() {
  local m
  for m in "$VM_ENVS"/*/manifest.env; do
    [ -f "$m" ] || continue
    if [ "$(vm_value "$m" DIR)" = "$root" ]; then printf '%s' "$m"; return 0; fi
  done
  return 0
}

# Pids whose cwd is inside this checkout and whose environment carries the run id.
vm_owned_pids() {
  local d p cwd
  for d in /proc/[0-9]*; do
    p="${d#/proc/}"
    [ "$p" != "$$" ] && [ "$p" != "$PPID" ] || continue
    cwd="$(readlink "$d/cwd" 2>/dev/null)" || continue
    case "$cwd/" in "$root"/*) ;; *) continue ;; esac
    tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -qxF "VERIFY_MULTICA_RUN_ID=$1" || continue
    printf '%s\n' "$p"
  done
}

vm_refuse() { echo "$(basename "$0"): refusing: $*" >&2; exit 3; }
