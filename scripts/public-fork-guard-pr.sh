#!/usr/bin/env bash
# CHE-9778: run the public-fork guard on one PR, as a step of the required `PR guard rules` check. Runs from the trusted
# workflow checkout (full history). The PR head is fetched as data and never checked out; title and body are read live from
# the API, so an edit that adds a workspace name is judged on the rerun that pr-guard-rules-edit.yml starts. Exit 1: the
# guard found a violation. Exit 2: the PR could not be read, so it was not judged.
# Env: PR_URL, REPO, GH_TOKEN for gh.
set -euo pipefail

num="${PR_URL:-}"
num="${num##*/}"
if [[ ! "$num" =~ ^[0-9]+$ ]]; then
  echo "::error::public-fork-guard: no PR number in PR_URL, so the PR cannot be judged."
  exit 2
fi

json="$(gh api "repos/${REPO:?}/pulls/$num")"
base="$(jq -r '.base.sha // empty' <<<"$json")"
head="$(jq -r '.head.sha // empty' <<<"$json")"
if [[ ! "$base" =~ ^[0-9a-f]{40}$ || ! "$head" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::public-fork-guard: the PR base or head commit is unreadable, so the PR cannot be judged."
  exit 2
fi
PR_TITLE="$(jq -r '.title // ""' <<<"$json")"
PR_BODY="$(jq -r '.body // ""' <<<"$json")"
export PR_TITLE PR_BODY

git fetch --no-tags -q origin "refs/pull/$num/head"
if [ "$(git rev-parse FETCH_HEAD)" != "$head" ]; then
  echo "::error::public-fork-guard: the PR head moved while the check ran; the newer push starts its own run."
  exit 2
fi
exec node "$(dirname "${BASH_SOURCE[0]}")/public-fork-guard.mjs" --base "$base" --head "$head"
