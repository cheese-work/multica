#!/usr/bin/env bash
set -euo pipefail

# CHE-2641 regression guard: one update source. On this fork the CLI, daemon,
# onboarding command and desktop bootstrap take their binary from our own
# server (/api/daemon/update, /api/daemon/install.sh), never from upstream
# multica-ai/multica releases or its install scripts.
#
# Fails on any non-test occurrence of
#   multica-ai/multica/releases
#   multica-ai/multica/main/scripts/install
# in packages, apps and server (*.ts, *.tsx, *.go). Docs, CHANGELOG, Go import
# paths (github.com/multica-ai/multica/server/...) and test files never match
# or are skipped. Allowlisted hits are listed below with the reason.

root="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

pattern='multica-ai/multica/(releases|main/scripts/install)'

# Exact repo-relative paths that may keep the upstream URL.
allowlist=(
  # Landing "download the desktop app" page: upstream desktop installers on
  # GitHub Releases, not a CLI/daemon update source.
  'apps/web/app/(landing)/download/download-client.tsx'
  'apps/web/features/landing/utils/github-release.ts'
  # Windows `multica update` (CHE-1012: Windows is out of scope for the
  # server-sourced update; Linux/macOS use update_fork.go).
  'server/internal/cli/update.go'
)

hits="$(cd "$root" && grep -rEn "$pattern" packages apps server \
  --include='*.ts' --include='*.tsx' --include='*.go' \
  --exclude='*.test.ts' --exclude='*.test.tsx' --exclude='*_test.go' \
  --exclude-dir=node_modules --exclude-dir=.next --exclude-dir=dist \
  --exclude-dir=out --exclude-dir=build --exclude-dir=.turbo 2>/dev/null || true)"

violations=""
while IFS= read -r line; do
  [ -n "$line" ] || continue
  file="${line%%:*}"
  skip=0
  for allowed in "${allowlist[@]}"; do
    [ "$file" = "$allowed" ] && skip=1
  done
  [ "$skip" -eq 1 ] || violations+="$line"$'\n'
done <<<"$hits"

if [ -n "$violations" ]; then
  echo "Upstream update source found. Use the server's /api/daemon/update and /api/daemon/install.sh instead:" >&2
  printf '%s' "$violations" >&2
  exit 1
fi
echo "check-no-upstream-update-source: ok"
