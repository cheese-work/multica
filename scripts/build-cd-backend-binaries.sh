#!/usr/bin/env bash
# Compile the CD backend binaries on the CI runner so the image build only
# packages them (CHE-7209), with a GHA Go cache at fixed paths (CHE-7211).
# Same commands, flags and stamps as the Dockerfile builder stages; the Go
# toolchain is the one the Dockerfile pins.
#
# Usage: build-cd-backend-binaries.sh guard   refuse a non-empty work dir, create it
#        build-cd-backend-binaries.sh [build] compile (default)
#
# Everything lives under one work dir, /tmp/multica-go-cd (CD_GO_BASE overrides it
# for tests): the source worktree src/, the toolchain go/, the module cache mod/,
# the build cache build/ and the output out/. The cache paths are the same on every
# runner, because Go keys cache objects by path and GitHub keys a cache entry by
# its paths. "guard" runs before the cache restore; the workflow removes the work
# dir when the job ends and the runner entrypoint wipes /tmp.
#
# Env (required): GITHUB_SHA VERSION BUILD GOCACHE GOMODCACHE   (build mode)
# Env (optional): CD_BACKEND_OUT   output dir (default $CD_GO_BASE/out)
#                 DATE             daemon build date stamp (default "unknown")
#                 CD_GO_EXTRA_FLAGS extra go flags, e.g. -x (default none)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "::error::build-cd-backend-binaries: $*" >&2; exit 1; }

mode="${1:-build}"
base="${CD_GO_BASE:-/tmp/multica-go-cd}"
case "$base" in
  /*) ;;
  *) fail "Go work dir $base is not an absolute path" ;;
esac
case "$base" in
  */../*|*/..|*/./*|*/.|//*|/) fail "Go work dir $base is not a normalized path" ;;
esac

if [ "$mode" = guard ]; then
  # A leftover work dir would carry a stale cache or toolchain into this job.
  if [ -e "$base" ] && { [ ! -d "$base" ] || [ -n "$(ls -A -- "$base")" ]; }; then
    fail "Go work dir $base is not empty: refusing to reuse a cache the job does not own"
  fi
  mkdir -p -- "$base"
  echo "guard: Go work dir $base is empty at start (runner ${RUNNER_NAME:-unknown})"
  exit 0
fi
[ "$mode" = build ] || fail "unknown mode '$mode' (use guard or build)"

: "${GITHUB_SHA:?GITHUB_SHA is required}"
: "${VERSION:?VERSION is required}"
: "${BUILD:?BUILD is required}"
COMMIT="$GITHUB_SHA"
DATE="${DATE:-unknown}"

# --- Path guard: runs before any download or compile. ---
[ "$root" = "$base/src" ] || fail "source tree $root is not $base/src: Go keys cache objects by path, so compiling elsewhere would defeat the cache"
for name in GOCACHE GOMODCACHE; do
  [ -n "${!name:-}" ] || fail "$name is not set: the Go cache must be a fixed directory under $base"
done
[ "$GOCACHE" = "$base/build" ] || fail "GOCACHE=$GOCACHE is not $base/build: the cache path must be the same on every runner"
[ "$GOMODCACHE" = "$base/mod" ] || fail "GOMODCACHE=$GOMODCACHE is not $base/mod: the cache path must be the same on every runner"
for dir in "$GOCACHE" "$GOMODCACHE"; do
  if [ -e "$dir" ] && [ ! -d "$dir" ]; then fail "$dir is not a directory"; fi
done
mkdir -p -- "$GOCACHE" "$GOMODCACHE"
export GOCACHE GOMODCACHE
echo "path guard: source $root, GOCACHE=$GOCACHE ($(find "$GOCACHE" -type f | wc -l) files), GOMODCACHE=$GOMODCACHE ($(find "$GOMODCACHE" -type f | wc -l) files), runner ${RUNNER_NAME:-unknown}"

out="${CD_BACKEND_OUT:-$base/out}"
case "$out" in "$base"/?*) ;; *) fail "CD_BACKEND_OUT=$out is outside $base" ;; esac
case "$out" in *..*) fail "CD_BACKEND_OUT=$out is not a normalized path" ;; esac
if [ -d "$out" ] && [ -n "$(ls -A -- "$out")" ]; then
  fail "CD_BACKEND_OUT=$out is not empty"
fi

# --- Go toolchain: the version of the Dockerfile builder image. ---
go_version="$(sed -n 's/^FROM golang:\([0-9][0-9.]*\)-alpine .*/\1/p' "$root/Dockerfile" | head -n 1)"
[ -n "$go_version" ] || fail "no 'FROM golang:<version>-alpine' line in Dockerfile"
# Checksums are pinned here (from https://go.dev/dl/) so a changed or poisoned
# download cannot pass. Raising the Dockerfile Go version needs a new line.
case "$go_version" in
  1.27.4) go_sha256=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840 ;;
  *) fail "no pinned checksum for Go $go_version: add it to scripts/build-cd-backend-binaries.sh" ;;
esac

mkdir -p "$base/dl" "$base/gopath" "$base/tmp" "$base/xdg-cache" "$base/xdg-config" "$out"
tarball="$base/dl/go${go_version}.linux-amd64.tar.gz"
curl -fsSL --retry 3 --retry-delay 2 --max-time 300 -o "$tarball" "https://go.dev/dl/go${go_version}.linux-amd64.tar.gz"
echo "${go_sha256}  ${tarball}" | sha256sum -c - >/dev/null || fail "Go ${go_version} tarball checksum mismatch"
# The tarball holds a top-level go/ directory: the toolchain lands in $base/go.
tar -xzf "$tarball" -C "$base"
rm -f "$tarball"

export PATH="$base/go/bin:$PATH"
export GOROOT="$base/go" GOTOOLCHAIN=local GOPATH="$base/gopath" GOTMPDIR="$base/tmp"
export XDG_CACHE_HOME="$base/xdg-cache" XDG_CONFIG_HOME="$base/xdg-config"
# One go build at a time, at most 3 compiles in it. -buildvcs=true stamps each
# binary with the checkout's vcs.revision and vcs.modified. -modcacherw keeps
# the module cache removable. CD_GO_EXTRA_FLAGS adds go flags (the local cache
# proof passes -x to count compile actions); the workflow leaves it unset.
export GOFLAGS="-p=3 -buildvcs=true -modcacherw${CD_GO_EXTRA_FLAGS:+ $CD_GO_EXTRA_FLAGS}"
go version | grep -F "go${go_version} " >/dev/null || fail "toolchain is not Go ${go_version}: $(go version)"

started=$SECONDS
cd "$root/server" && go mod download

(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/migrate" ./cmd/migrate)
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/maintenance" ./cmd/maintenance)
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/backfill_task_usage_hourly" ./cmd/backfill_task_usage_hourly)
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/backfill_codex_usage_cache" ./cmd/backfill_codex_usage_cache)
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/backfill_issue_created_via" ./cmd/backfill_issue_created_via)
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w -X main.version=${VERSION} -X main.build=${BUILD} -X main.commit=${COMMIT}" -o "$out/server" ./cmd/server)

# The CLI and the daemon builds served to daemons come from one script, so the
# image's own multica and the downloadable ones cannot drift apart.
VERSION="$VERSION" BUILD="$BUILD" COMMIT="$COMMIT" DATE="$DATE" sh "$root/scripts/build-daemon-binaries.sh" "$out/daemon-binaries"
cp "$out/daemon-binaries/multica-linux-amd64" "$out/multica"

# Checksums of every file the image copies; the image build verifies them.
(cd "$out" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | xargs sha256sum > SHA256SUMS)
printf 'VERSION=%q\nBUILD=%q\n' "$VERSION" "$BUILD" > "$base/stamp.env"

echo "compile seconds: $((SECONDS - started))"
echo "runner: ${RUNNER_NAME:-unknown}"
du -sb "$GOCACHE" "$GOMODCACHE" | sed 's/^/cache bytes: /'
echo "binaries sha256 (SHA256SUMS): $(sha256sum "$out/SHA256SUMS" | cut -d' ' -f1)"
