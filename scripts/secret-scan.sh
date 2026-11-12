#!/usr/bin/env bash
# Scan the commits in a git revision range for secrets (CHE-2696). Usage: secret-scan.sh <git-log-range|-1 SHA>
# Needs the repo checked out with full history. Findings print redacted. Add `gitleaks:allow` to a line to
# accept a deliberate fixture. History before this gate is not scanned; only the range you pass.
set -euo pipefail
V=8.30.1
SHA256=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb  # gitleaks_${V}_linux_x64.tar.gz
range="${1:?usage: secret-scan.sh <range>}"

bin="${GITLEAKS_BIN:-}"
if [[ -z "$bin" ]]; then
  d="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/gitleaks-$V"; bin="$d/gitleaks"
  # The tarball is the cache unit (CI caches $d): it is re-verified on every run, so a restored cache is never trusted blindly.
  "$(dirname "$0")/fetch-verified.sh" "https://github.com/gitleaks/gitleaks/releases/download/v$V/gitleaks_${V}_linux_x64.tar.gz" "$SHA256" "$d/gl.tgz"
  tar -xzf "$d/gl.tgz" -C "$d" gitleaks
fi
exec "$bin" git --no-banner --redact --log-opts="$range" .
