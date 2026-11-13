#!/usr/bin/env bash
# Replay check for brief-shrink PRs (CHE-4574). Run on X99 (it builds Go).
#   scripts/brief-replay.sh <old-ref> <new-ref> <checklist>
# Renders the generated brief at both refs for fixed task shapes, then diffs rules
# against the checklist (scripts/brief-replay-check.mjs). Exit 1 = a checklist rule
# was lost. Static text check only: it does not prove model behavior.
# Real-run evidence for the old side (no Go build):
#   node scripts/brief-replay-check.mjs --real <checklist> /mnt/ssd2/multica_workspaces [recent-count=20]
# Each checklist phrase must be in at least one of the newest finished run briefs (CLAUDE.md/AGENTS.md).
set -euo pipefail
[ $# -eq 3 ] || { echo "usage: $0 <old-ref> <new-ref> <checklist>" >&2; exit 2; }
repo=$(git rev-parse --show-toplevel)
checklist=$(realpath "$3")
tmp=$(mktemp -d)
cleanup() {
  for w in old new; do git -C "$repo" worktree remove --force "$tmp/wt-$w" 2>/dev/null || true; done
  rm -rf "$tmp"
}
trap cleanup EXIT
for side in old new; do
  ref=$1; [ "$side" = new ] && ref=$2
  git -C "$repo" worktree add -q --detach "$tmp/wt-$side" "$ref" >/dev/null
  mkdir "$tmp/$side"
  cp "$repo/scripts/brief-replay/render_test.go.txt" "$tmp/wt-$side/server/internal/daemon/execenv/zz_brief_replay_render_test.go"
  (cd "$tmp/wt-$side/server" && BRIEF_REPLAY_OUT="$tmp/$side" go test ./internal/daemon/execenv -run '^TestBriefReplayRender$' -count=1 >"$tmp/go-$side.log" 2>&1) \
    || { cat "$tmp/go-$side.log" >&2; echo "render failed at $ref" >&2; exit 2; }
  echo "rendered $ref: $(cat "$tmp/$side"/*.md | wc -c) bytes in $(ls "$tmp/$side" | wc -l) fixtures"
done
node "$repo/scripts/brief-replay-check.mjs" "$tmp/old" "$tmp/new" "$checklist"
