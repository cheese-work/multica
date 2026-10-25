#!/usr/bin/env bash
# Writes current=true|false: whether <source_sha> is still the head of main.
# A superseded run is skipped, never failed; an API error fails closed.
set -euo pipefail

source_sha=$1

main_sha="$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/main" --jq '.object.sha')"
if [ "$source_sha" = "$main_sha" ]; then
  echo 'current=true' >> "$GITHUB_OUTPUT"
else
  echo 'current=false' >> "$GITHUB_OUTPUT"
  printf 'Skipped before C00 mutation: candidate `%s` is superseded by main `%s`.\n' \
    "$source_sha" "$main_sha" >> "$GITHUB_STEP_SUMMARY"
fi
