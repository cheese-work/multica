#!/bin/bash
# One scheduled poll (CHE-3958). Keeps a private checkout of the ledger and script,
# refreshed from origin each run, then runs the watcher. Stale/backoff exits 1.
set -euo pipefail
ref="${UPSTREAM_WATCH_REF:-main}"
dir="${UPSTREAM_WATCH_DIR:-$HOME/.local/share/upstream-pr-watch/multica}"
if [ ! -d "$dir/.git" ]; then
  mkdir -p "$(dirname "$dir")"
  git clone --quiet --depth 1 --branch "$ref" https://github.com/cheese-work/multica.git "$dir"
fi
git -C "$dir" fetch --quiet --depth 1 origin "$ref"
git -C "$dir" checkout --quiet --force FETCH_HEAD
cd "$dir"
exec node scripts/upstream-pr-watch.mjs --ledger docs/fork/UPSTREAM-PRS.md
