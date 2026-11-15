#!/usr/bin/env bash
set -euo pipefail

# CHE-3955: fast-forward TARGET_URL branch upstream-main to UPSTREAM_URL main.
# Never forces. Creates the branch when missing; no-op when equal; fails when
# upstream main is not a descendant of the mirror (history rewritten).

upstream_url="${UPSTREAM_URL:-https://github.com/multica-ai/multica.git}"
target_url="${TARGET_URL:?TARGET_URL required}"
branch="${MIRROR_BRANCH:-upstream-main}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
git init -q "$tmp"
cd "$tmp"

git fetch -q --no-tags "$upstream_url" "+refs/heads/main:refs/remotes/upstream/main"
up="$(git rev-parse refs/remotes/upstream/main)"

if mirror="$(git ls-remote --exit-code "$target_url" "refs/heads/$branch" | cut -f1)"; then
  if [ "$mirror" = "$up" ]; then
    echo "$branch up to date at $up"
    exit 0
  fi
  git fetch -q --no-tags "$target_url" "+refs/heads/$branch:refs/remotes/target/$branch"
  if ! git merge-base --is-ancestor "$mirror" "$up"; then
    echo "ERROR: upstream main $up is not a fast-forward of $branch $mirror; refusing to update" >&2
    exit 1
  fi
else
  mirror="(none)"
fi

git push -q "$target_url" "$up:refs/heads/$branch"
echo "$branch updated $mirror -> $up"
