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
rc=0
node "$script" --ledger docs/fork/UPSTREAM-PRS.md || rc=$?
# CHE-3960: weekly ledger stale check; a stale result files one issue (deduped for 7 days), so an owner hears of it even when the fork
# saw no push. Ids: dev team squad, Cheese Work Setup project. Exit non-zero if either step failed (a stale ledger is not a failure once filed).
# A pinned ref from before this script landed has no stale check: skip it.
if [ -f scripts/fork-ledger-stale.mjs ]; then
  node scripts/fork-ledger-stale.mjs --file "${FORK_LEDGER_ASSIGNEE:-a1073a68-4965-44ff-a8fb-244341935110}" "${FORK_LEDGER_PROJECT:-5e621e12-eeb4-40a5-9571-25657d5b0ec5}" || rc=$?
fi
exit "$rc"
