#!/usr/bin/env bash
# CHE-9542: turn the sync freeze on safely. Run by a human with repo-variable write access (agent tokens get 403):
#   REPO=cheese-work/multica bash scripts/sync-freeze-activate.sh
# Why a script and not a bare `gh variable set`: a merge-queue entry that already passed its checks keeps that result.
# Setting SYNC_FREEZE=on does not re-run it, so a non-urgent PR queued before the set can still merge. The barrier:
#   1. refuse while the queue holds any entry other than PR 1834 (drain it first: let it merge or dequeue it);
#   2. set the variable;
#   3. read the queue again. Any entry other than PR 1834 now joined before the set, so its check ran with the old value:
#      exit 2 and name it. Entries that join after step 2 are judged by the freeze rule (scripts/sync-freeze-check.sh).
# Env: REPO (default cheese-work/multica), BASE (default main), SYNC_FREEZE_ALLOWED_PR (default 1834).
set -euo pipefail

repo="${REPO:-cheese-work/multica}"
base="${BASE:-main}"
allowed="${SYNC_FREEZE_ALLOWED_PR:-1834}"
owner="${repo%%/*}"
name="${repo#*/}"
query='query($owner:String!,$name:String!,$branch:String!){repository(owner:$owner,name:$name){mergeQueue(branch:$branch){entries(first:100){totalCount nodes{pullRequest{number}}}}}}'

# Prints the queued PR numbers other than the allowed one, one per line. Fail closed: an unreadable or truncated queue is an error.
queued_others() {
  local out total nodes
  out="$(gh api graphql -f query="$query" -F owner="$owner" -F name="$name" -F branch="$base" 2>&1)" || {
    echo "Cannot read the merge queue of $repo ($(printf '%s' "$out" | head -n 1 | cut -c1-200)); nothing changed." >&2
    return 1
  }
  total="$(printf '%s' "$out" | grep -o '"totalCount":[0-9]*' | head -n 1 | cut -d: -f2)"
  nodes="$(printf '%s' "$out" | grep -o '"number":[0-9]*' | cut -d: -f2 || true)"
  if [ -z "$total" ] || [ "$total" -gt "$(printf '%s' "$nodes" | grep -c .)" ]; then
    echo "The merge queue read is incomplete or missing (totalCount=${total:-none}); nothing changed." >&2
    return 1
  fi
  printf '%s\n' "$nodes" | grep -vxF "$allowed" | grep . || true
}

others="$(queued_others)" || exit 1
if [ -n "$others" ]; then
  echo "Refusing to turn the freeze on: the merge queue still holds PR(s) $(printf '%s' "$others" | tr '\n' ' ')." >&2
  echo "Their checks ran with the freeze off and stay valid. Drain the queue first (let them merge or dequeue them), then run this again." >&2
  exit 1
fi

gh variable set SYNC_FREEZE --body on -R "$repo"

late="$(queued_others)" || { echo "Variable is on but the queue could not be re-read. Rollback: gh variable set SYNC_FREEZE --body off -R $repo" >&2; exit 2; }
if [ -n "$late" ]; then
  echo "SYNC_FREEZE is on, but PR(s) $(printf '%s' "$late" | tr '\n' ' ')joined the merge queue before the set; their checks ran with the old value and can still merge." >&2
  echo "Dequeue them now. Rollback of the freeze: gh variable set SYNC_FREEZE --body off -R $repo" >&2
  exit 2
fi
echo "SYNC_FREEZE is on for $repo; the merge queue holds nothing but PR $allowed or is empty."
echo "Lift: gh variable set SYNC_FREEZE --body off -R $repo"
