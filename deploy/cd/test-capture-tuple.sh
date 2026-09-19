#!/usr/bin/env bash
set -euo pipefail

# Regression test for capture-tuple.sh's application_sha resolution (CHE-530).
#
# The first real D2 run on the cheese-c00-deploy runner failed here: C00's
# running images were not built by D1, carry no org.opencontainers.image.revision
# label, and are tagged with a short SHA. capture-tuple.sh demanded a
# 40-character SHA and had no way to get one, so the whole deploy chain stopped
# at its first step.
#
# Covered, in the order capture-tuple.sh tries them:
#   1. the image's own revision label wins when present
#   2. a `sha-<40>` tag (what D1 publishes) is used directly
#   3. a short tag is expanded against a real git repository
#   4. an unresolvable tag fails loudly rather than inventing a SHA
#
# docker and the compose calls are scripted mocks; the git repository is real,
# because expanding an abbreviated commit is exactly the behaviour under test
# and mocking it would test nothing.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

mock_bin="$work_dir/bin"
mkdir -p "$mock_bin"

cat >"$mock_bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

if [ "$1" = "inspect" ]; then
  prev=""; fmt=""
  for a in "$@"; do
    [ "$prev" = "--format" ] && fmt="$a"
    prev="$a"
  done
  case "$fmt" in
    *config-hash*) printf '%s\n' "aaaabbbbccccddddeeeeffff00001111222233334444555566667777888899990" ;;
    *image.revision*) printf '%s\n' "${MOCK_IMAGE_REVISION:-}" ;;
    *RepoDigests*) printf 'repo@%s\n' "${MOCK_DIGEST:?}" ;;
    *.Id*) printf '%s\n' "${MOCK_DIGEST:?}" ;;
  esac
  exit 0
fi

if [ "$1" = "compose" ]; then
  shift
  [ "$1" = "-f" ] && shift 2
  sub="$1"; shift
  case "$sub" in
    images)
      case "${1:-backend}" in
        backend) printf '[{"Repository":"cheese-work/multica-backend","Tag":"%s"}]\n' "${MOCK_IMAGE_TAG:?}" ;;
        frontend) printf '[{"Repository":"cheese-work/multica-web","Tag":"%s"}]\n' "${MOCK_IMAGE_TAG:?}" ;;
        *) printf '[]\n' ;;
      esac
      exit 0 ;;
    ps) printf 'mock-container\n'; exit 0 ;;
    exec)
      sql="${*: -1}"
      case "$sql" in
        *"count(*)"*) printf '529|495_agent_task_rerun_lineage_unique|2026-09-16T01:51:45Z\n' ;;
        *) printf '495_agent_task_rerun_lineage_unique\n' ;;
      esac
      exit 0 ;;
  esac
fi
echo "mock docker: unhandled $*" >&2
exit 1
MOCK
chmod +x "$mock_bin/docker"
export PATH="$mock_bin:$PATH"
export MOCK_DIGEST="sha256:$(printf 'a%.0s' {1..64})"

compose_dir="$work_dir/compose"
mkdir -p "$compose_dir"
printf 'mock compose file\n' >"$compose_dir/docker-compose.selfhost.yml"

# A real repository with a real commit, so the abbreviation lookup is genuine.
repo="$work_dir/repo"
git init -q "$repo"
git -C "$repo" -c user.email=t@t -c user.name=t commit -q --allow-empty -m seed
full_sha="$(git -C "$repo" rev-parse HEAD)"
short_sha="${full_sha:0:8}"

capture() {
  bash deploy/cd/capture-tuple.sh --compose-dir "$compose_dir" --output "$work_dir/out.json" "$@"
}

sha_of() {
  node -e 'process.stdout.write(JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")).application_sha)' "$work_dir/out.json"
}

fail() { echo "FAIL: $1" >&2; exit 1; }

# --- 1. the revision label wins -------------------------------------------
label_sha="$(printf 'b%.0s' {1..40})"
MOCK_IMAGE_REVISION="$label_sha" MOCK_IMAGE_TAG="$short_sha" capture --git-dir "$repo" >/dev/null
[ "$(sha_of)" = "$label_sha" ] || fail "revision label should win over the tag"

# --- 2. a D1-style sha-<40> tag is used directly when there is no repo ------
# --git-dir "" disables the default self-repository lookup, modelling the real
# C00 invocation: the script runs from a bare scp'd temp directory with no git.
MOCK_IMAGE_REVISION="" MOCK_IMAGE_TAG="sha-$full_sha" capture --git-dir "" >/dev/null
[ "$(sha_of)" = "$full_sha" ] || fail "sha-<40> tag should resolve without a git dir"

# --- 3. a short tag expands against the repository -------------------------
# This is the case that broke the first real deploy.
MOCK_IMAGE_REVISION="" MOCK_IMAGE_TAG="$short_sha" capture --git-dir "$repo" >/dev/null
[ "$(sha_of)" = "$full_sha" ] || fail "short tag should expand to the full commit"

# --- 3b. a full-length tag is VERIFIED when a repository is available -------
# Review finding: a syntactically valid sha-<40 hex> tag is not proof the
# commit exists. When we have a repository to check against, a tag naming a
# commit that is not in it must be refused, not recorded.
absent_sha="$(printf 'c%.0s' {1..40})"
if MOCK_IMAGE_REVISION="" MOCK_IMAGE_TAG="sha-$absent_sha" capture --git-dir "$repo" >/dev/null 2>&1; then
  fail "a sha-<40> tag absent from the repository must be refused when --git-dir is given"
fi
# ...and the same tag is still accepted when there is no repository to consult,
# since a full SHA is self-describing and refusing would break the D1 path.
MOCK_IMAGE_REVISION="" MOCK_IMAGE_TAG="sha-$absent_sha" capture --git-dir "" >/dev/null
[ "$(sha_of)" = "$absent_sha" ] || fail "sha-<40> tag should still be used when no repository is available"

# --- 4. an unresolvable tag fails loudly -----------------------------------
# "latest" is not a commit; recording anything here would be a fabricated
# baseline, which is worse than refusing.
if MOCK_IMAGE_REVISION="" MOCK_IMAGE_TAG="latest" capture --git-dir "$repo" >/dev/null 2>&1; then
  fail "an unresolvable tag must not produce a tuple"
fi

# A short tag with no git dir to resolve it must also refuse, not guess.
if MOCK_IMAGE_REVISION="" MOCK_IMAGE_TAG="$short_sha" capture --git-dir "" >/dev/null 2>&1; then
  fail "a short tag with no repository must not produce a tuple"
fi

echo "capture-tuple.sh application_sha fixtures passed"
