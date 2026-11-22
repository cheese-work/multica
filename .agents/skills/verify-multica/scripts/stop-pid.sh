#!/usr/bin/env bash
# Stop one process this run started. Usage: stop-pid.sh <pid> <expected-dir>
# Refuses (exit 2) unless the pid is numeric, is not the multica daemon or CLI,
# and its cwd is inside <expected-dir>. Never matches by process name.
set -euo pipefail
pid="${1:-}"; want="${2:-}"
refuse() { echo "stop-pid: refusing pid '$pid': $1" >&2; exit 2; }
[[ "$pid" =~ ^[0-9]+$ ]] || refuse "not a numeric pid"
[ -n "$want" ] || refuse "no expected directory"
[ -d "/proc/$pid" ] || { echo "stop-pid: pid $pid already gone"; exit 0; }
comm="$(cat "/proc/$pid/comm" 2>/dev/null || true)"
case "$comm" in multica|multica.exe) refuse "it is a multica binary (possibly the daemon)";; esac
cwd="$(readlink "/proc/$pid/cwd" 2>/dev/null || true)"
want="$(cd "$want" && pwd -P)"
case "$cwd/" in "$want"/*) ;; *) refuse "cwd '$cwd' is outside '$want'";; esac
kill -TERM "$pid"
for _ in $(seq 1 20); do [ -d "/proc/$pid" ] || { echo "stop-pid: stopped $pid"; exit 0; }; sleep 0.5; done
kill -KILL "$pid"; echo "stop-pid: killed $pid"
