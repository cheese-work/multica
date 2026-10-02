#!/usr/bin/env bash
# Prints the version to stamp into a CI daemon artifact (main.version).
#
# The daemon advertises this string as CLIVersion, and the Quick Create /
# capability gates (packages/core/runtimes/cli-version.ts and
# server/pkg/agent/version.go) accept only a semver tag or git-describe output
# (`v0.6.0-153-ged9f2e94d5`). The exact build commit is stamped separately
# (main.commit) and is what the updater verifies; this string only has to stay
# in the supported shape. Fails closed rather than emit an unsupported version.
set -euo pipefail

version=$(git describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --exclude 'v*-*' --abbrev=12 2>/dev/null) || {
  echo "daemon-artifact-version: no vX.Y.Z tag reachable from HEAD (checkout needs fetch-depth: 0)" >&2
  exit 1
}
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+-g[0-9a-f]+)?$ ]]; then
  echo "daemon-artifact-version: unsupported version shape: $version" >&2
  exit 1
fi
printf '%s\n' "$version"
