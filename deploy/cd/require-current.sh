#!/usr/bin/env bash
set -euo pipefail

source_sha=${1:?candidate SHA is required}
[[ "$source_sha" =~ ^[a-f0-9]{40}$ ]]

skip_candidate() {
  echo 'current=false' >> "$GITHUB_OUTPUT"
  printf 'Skipped before C00 approval: candidate `%s` %s `%s`.\n' \
    "$source_sha" "$1" "$2" >> "$GITHUB_STEP_SUMMARY"
  exit 0
}

main_sha="$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/main" --jq '.object.sha')"
[[ "$main_sha" =~ ^[a-f0-9]{40}$ ]]
if [ "$source_sha" != "$main_sha" ]; then
  status="$(gh api "repos/${GITHUB_REPOSITORY}/compare/${source_sha}...${main_sha}?per_page=1" --jq '.status')"
  case "$status" in
    ahead) ;;
    behind|diverged|identical) skip_candidate 'is not an ancestor of main' "$main_sha" ;;
    *) echo "Unknown main comparison status: $status" >&2; exit 1 ;;
  esac
fi

live_version="$(curl --proto '=https' --fail --silent --show-error --max-time 15 "${C00_CONFIG_URL:?C00 config URL is required}" \
  | node -e 'let data=""; process.stdin.on("data", chunk => data += chunk); process.stdin.on("end", () => {
    const commit = JSON.parse(data).server_commit;
    if (typeof commit !== "string" || !/^(?:[a-f0-9]{7}|[a-f0-9]{40})$/.test(commit)) {
      throw new Error("C00 did not report a known commit");
    }
    process.stdout.write(commit);
  });')"
live_sha="$live_version"
if [ "${#live_sha}" -ne 40 ]; then
  # A 7-char prefix can be ambiguous across the fork network (GitHub API 422); the checkout is full-depth.
  live_sha="$(git rev-parse --verify --quiet "${live_version}^{commit}" 2>/dev/null \
    || gh api "repos/${GITHUB_REPOSITORY}/commits/${live_version}" --jq '.sha')"
fi
[[ "$live_sha" =~ ^[a-f0-9]{40}$ && "$live_sha" == "$live_version"* ]]
[ "$source_sha" != "$live_sha" ] || skip_candidate 'is already running on C00' "$live_sha"

status="$(gh api "repos/${GITHUB_REPOSITORY}/compare/${live_sha}...${source_sha}?per_page=1" --jq '.status')"
case "$status" in
  ahead) ;;
  behind|identical|diverged) skip_candidate 'does not move forward from C00' "$live_sha" ;;
  *) echo "Unknown C00 comparison status: $status" >&2; exit 1 ;;
esac

echo 'current=true' >> "$GITHUB_OUTPUT"
