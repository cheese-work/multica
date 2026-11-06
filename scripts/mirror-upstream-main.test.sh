#!/usr/bin/env bash
set -euo pipefail

# CHE-3955: fast-forward mirror of upstream main into the fork's upstream-main.
# Local bare-repo fixture; covers create, advance, no-op, rewrite-stops.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mirror="$root_dir/scripts/mirror-upstream-main.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
fail() { echo "FAIL: $*" >&2; exit 1; }

git init -q --bare -b main "$work/upstream.git"
git init -q --bare -b main "$work/fork.git"
git init -q -b main "$work/src"
commit() { git -C "$work/src" commit -q --allow-empty -m "$1"; }
push_up() { git -C "$work/src" push -q "$@" "$work/upstream.git" main; }
up_sha() { git -C "$work/upstream.git" rev-parse main; }
fork_sha() { git -C "$work/fork.git" rev-parse refs/heads/upstream-main; }
run() { UPSTREAM_URL="$work/upstream.git" TARGET_URL="$work/fork.git" bash "$mirror"; }

commit one; commit two; push_up

# First run creates upstream-main at the upstream SHA.
run >/dev/null || fail "first run must succeed"
[ "$(fork_sha)" = "$(up_sha)" ] || fail "first run must create upstream-main at upstream main"

# Second run is a no-op.
before="$(fork_sha)"
run | grep -q "up to date" || fail "second run must report up to date"
[ "$(fork_sha)" = "$before" ] || fail "no-op must not move the branch"

# Upstream advances: fast-forward.
commit three; push_up
run >/dev/null || fail "advance must succeed"
[ "$(fork_sha)" = "$(up_sha)" ] || fail "advance must fast-forward to upstream main"

# Upstream rewrites history: stop, fail, branch unchanged.
before="$(fork_sha)"
git -C "$work/src" reset -q --hard HEAD~2; commit rewritten; push_up -f
if run >"$work/out" 2>&1; then fail "rewrite must fail"; fi
grep -q "not a fast-forward" "$work/out" || fail "rewrite must name the reason"
[ "$(fork_sha)" = "$before" ] || fail "rewrite must not move the branch"

echo "ok"
