#!/bin/bash
# One scheduled poll (CHE-3958). Keeps a private checkout of the ledger and script,
# refreshed from origin each run, then runs the watcher. Stale/backoff exits 1.
#
# Runs main. UPSTREAM_WATCH_REF pins another branch only while main does not carry the
# watcher yet (before its PR lands) or while that branch exists; once main has the script,
# or the pinned branch is gone, main is used, so a leftover drop-in cannot freeze the ledger.
set -euo pipefail
ref="${UPSTREAM_WATCH_REF:-main}"
remote="${UPSTREAM_WATCH_REMOTE:-https://github.com/cheese-work/multica.git}"
dir="${UPSTREAM_WATCH_DIR:-$HOME/.local/share/upstream-pr-watch/multica}"
script=scripts/upstream-pr-watch.mjs
if [ ! -d "$dir/.git" ]; then
  mkdir -p "$dir"
  git -C "$dir" init --quiet
fi
git -C "$dir" remote remove origin 2>/dev/null || true
git -C "$dir" remote add origin "$remote"
git -C "$dir" fetch --quiet --depth 1 origin main
main_sha="$(git -C "$dir" rev-parse FETCH_HEAD)"
target="$main_sha"
if [ "$ref" != main ] && ! git -C "$dir" cat-file -e "$main_sha:$script" 2>/dev/null; then
  if git -C "$dir" fetch --quiet --depth 1 origin "$ref" 2>/dev/null; then
    target="$(git -C "$dir" rev-parse FETCH_HEAD)"
  else
    echo "upstream-watch: pinned ref $ref not found, using main" >&2
  fi
fi
git -C "$dir" checkout --quiet --force "$target"
cd "$dir"
exec node "$script" --ledger docs/fork/UPSTREAM-PRS.md
