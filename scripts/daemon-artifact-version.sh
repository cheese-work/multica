#!/usr/bin/env bash
# Prints the version to stamp into a CI daemon artifact (main.version).
#
# The daemon advertises this string as CLIVersion, and the Quick Create /
# capability gates (packages/core/runtimes/cli-version.ts and
# server/pkg/agent/version.go) parse the leading X.Y.Z semver core while
# server/internal/buildinfo/buildinfo.go splits the trailing ICT build stamp
# (`0.6.3-20261021-0856`). The exact build commit is stamped separately
# (main.commit) and is what the updater verifies. Fails closed rather than emit
# an unsupported version.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(git rev-parse --show-toplevel 2>/dev/null || (cd "$script_dir/.." && pwd))
upstream_file="$repo_root/UPSTREAM_VERSION"

if [[ ! -f "$upstream_file" ]]; then
  echo "daemon-artifact-version: missing UPSTREAM_VERSION at $upstream_file" >&2
  exit 1
fi

upstream_version=$(tr -d '[:space:]' < "$upstream_file")
if ! [[ "$upstream_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "daemon-artifact-version: invalid UPSTREAM_VERSION: $upstream_version" >&2
  exit 1
fi

commit_ref="${1:-HEAD}"
commit_time=$(git show -s --format=%cI "$commit_ref" 2>/dev/null) || {
  echo "daemon-artifact-version: unable to read commit timestamp for $commit_ref" >&2
  exit 1
}

version=$(sh "$script_dir/release-stamp.sh" "$upstream_version" "$commit_time")
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+-[0-9]{8}-[0-9]{4}$ ]]; then
  echo "daemon-artifact-version: unsupported version shape: $version" >&2
  exit 1
fi
printf '%s\n' "$version"
