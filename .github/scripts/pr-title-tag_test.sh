#!/usr/bin/env bash
# Tests for pr-title-tag.sh. Exit 1 on any wrong verdict.
check="$(dirname "$0")/pr-title-tag.sh"
fail=0
accept() { bash "$check" "$1" 2>/dev/null || { echo "FAIL: should accept: '$1'"; fail=1; }; }
reject() { ! bash "$check" "$1" 2>/dev/null || { echo "FAIL: should reject: '$1'"; fail=1; }; }

accept 'feat(x): add y (CHE-1) [squash]'
accept 'chore(upstream): sync google/artemis release [merge]'
reject 'feat(x): add y (CHE-1)'
reject 'feat(x): add y [squash] [merge]'
reject 'feat(x): add y [merge][merge]'
reject 'feat(x): [merge] add y [squash]'
reject '[squash] feat(x): add y'
reject 'feat(x): add y [Squash]'
reject 'feat(x): add y [rebase]'
reject ''

[[ $fail -eq 0 ]] && echo "pr-title-tag tests: OK"
exit $fail
