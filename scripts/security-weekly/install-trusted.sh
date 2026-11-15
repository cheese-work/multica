#!/usr/bin/env bash
# CHE-3701: owner-run, on X99. Install the Monday-brief toolchain from a reviewed revision of origin/main, outside any checkout.
# The consumer holds the secret key, so weekly.sh runs only from here, never from a working tree a PR could have changed.
# Usage (inside a cheese-work/multica checkout): install-trusted.sh <sha>
set -euo pipefail
sha="$(git rev-parse --verify "$1^{commit}")"
git fetch --quiet origin main
git merge-base --is-ancestor "$sha" origin/main || { echo "install-trusted: $sha is not on origin/main" >&2; exit 1; }
dest="${SECURITY_WEEKLY_STATE:-/mnt/ssd2/security-weekly}/trusted"
mkdir -p "$dest/$sha"
git archive "$sha" scripts/security-weekly | tar -x --strip-components=2 -C "$dest/$sha"
ln -sfn "$dest/$sha" "$dest/current"
echo "trusted toolchain: $dest/current -> $sha"
