#!/usr/bin/env bash
# Pass when a PR title ends with exactly one merge-method tag: [squash] or [merge].
# Rule: cheese-work/multica-dotfiles standards/general/commit-conventions.md (CHE-1678).
title=${1-}
count=$(grep -oE '\[(squash|merge)\]' <<<"$title" | wc -l)
if [[ $title =~ \[(squash|merge)\]$ && $count -eq 1 ]]; then
  exit 0
fi
echo "PR title must end with exactly one [squash] or [merge] tag, got: $title" >&2
exit 1
