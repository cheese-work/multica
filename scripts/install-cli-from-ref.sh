#!/usr/bin/env bash
# Build the `multica` CLI from an exact commit of a given repo and install it
# to an authenticated daemon host's local bin directory.
#
# `multica update` and scripts/install.sh both pull from multica-ai/multica's
# PUBLIC GitHub Releases — there is no automated path from a merged commit on
# a private fork (e.g. cheese-work/multica) to an authenticated daemon host's
# `~/.local/bin/multica`. CD (cd-deploy.yml) only ever updates C00's backend
# container; it never touches a daemon host's own CLI install. Until now the
# only way to get a fork-only commit onto a daemon host was the manual
# `make build` a preflight would run by hand (see CHE-765).
#
# Usage:
#   scripts/install-cli-from-ref.sh --repo <git-url> --ref <commit-or-tag> [--bin-dir <dir>]
#
# Verifies the installed binary reports the expected commit before exiting
# successfully, so a partial or stale install is never reported as done.
set -euo pipefail

usage() {
  echo "usage: install-cli-from-ref.sh --repo <git-url> --ref <commit-or-tag> [--bin-dir <dir>]" >&2
}

repo_url=""
ref=""
bin_dir="${MULTICA_BIN_DIR:-$HOME/.local/bin}"

while (($#)); do
  case "$1" in
    --repo) repo_url=${2:?}; shift 2 ;;
    --ref) ref=${2:?}; shift 2 ;;
    --bin-dir) bin_dir=${2:?}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done
[ -n "$repo_url" ] || { usage; exit 2; }
[ -n "$ref" ] || { usage; exit 2; }

command -v go >/dev/null 2>&1 || { echo "install-cli-from-ref: go is required" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "install-cli-from-ref: git is required" >&2; exit 1; }

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

echo "Cloning $repo_url at $ref..."
git clone --quiet "$repo_url" "$work_dir/src"
git -C "$work_dir/src" checkout --quiet "$ref"
resolved_commit="$(git -C "$work_dir/src" rev-parse HEAD)"

echo "Building multica CLI from $resolved_commit..."
(
  cd "$work_dir/src/server"
  go build -ldflags "-X main.version=$ref -X main.commit=${resolved_commit:0:9} -X main.date=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    -o "$work_dir/multica" ./cmd/multica
)

mkdir -p "$bin_dir"
install -m 0755 "$work_dir/multica" "$bin_dir/multica"

installed_commit="$("$bin_dir/multica" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
if [ "${resolved_commit:0:9}" != "$installed_commit" ]; then
  echo "install-cli-from-ref: installed binary reports commit $installed_commit, expected ${resolved_commit:0:9}" >&2
  exit 1
fi

echo "Installed multica ($installed_commit) to $bin_dir/multica"
