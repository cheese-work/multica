#!/usr/bin/env bash
# Compile the CD backend binaries on the CI runner so the image build only
# packages them (CHE-7209). Same commands, flags and stamps as the Dockerfile
# builder stages; the Go toolchain is the one the Dockerfile pins.
#
# Env (required): RUNNER_TEMP GITHUB_SHA VERSION BUILD GOCACHE GOMODCACHE
# Env (optional): CD_BACKEND_OUT   output dir (default $RUNNER_TEMP/cd-go/out)
#                 DATE             daemon build date stamp (default "unknown")
#
# GOCACHE and GOMODCACHE must be empty directories inside $RUNNER_TEMP: a job
# never reads a cache another job wrote, and nothing lands in ~/.cache, ~/go or
# the runner workspace root (CHE-2557). The workflow removes $RUNNER_TEMP/cd-go
# when the job ends.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "::error::build-cd-backend-binaries: $*" >&2; exit 1; }

: "${RUNNER_TEMP:?RUNNER_TEMP is required}"
: "${GITHUB_SHA:?GITHUB_SHA is required}"
: "${VERSION:?VERSION is required}"
: "${BUILD:?BUILD is required}"
COMMIT="$GITHUB_SHA"
DATE="${DATE:-unknown}"

temp="$(realpath -m -- "$RUNNER_TEMP")"
base="$temp/cd-go"

# --- Cache guard: runs before any download or compile. ---
guard_cache() {
  local name="$1" value resolved
  value="${!name:-}"
  [ -n "$value" ] || fail "$name is not set: the Go cache must be a job-scoped directory under RUNNER_TEMP"
  resolved="$(realpath -m -- "$value")"
  case "$resolved" in
    "$temp"/?*) ;;
    *) fail "$name=$value is outside RUNNER_TEMP ($temp): refusing to use a cache the job does not own" ;;
  esac
  if [ -e "$resolved" ] && [ ! -d "$resolved" ]; then
    fail "$name=$value is not a directory"
  fi
  if [ -d "$resolved" ] && [ -n "$(ls -A -- "$resolved")" ]; then
    fail "$name=$value is not empty: the Go cache must be empty at the start of the job"
  fi
  mkdir -p -- "$resolved"
  printf -v "$name" '%s' "$resolved"
  echo "cache guard: $name=$resolved is empty at start (runner ${RUNNER_NAME:-unknown})"
}
guard_cache GOCACHE
guard_cache GOMODCACHE
export GOCACHE GOMODCACHE
[ "$GOCACHE" != "$GOMODCACHE" ] || fail "GOCACHE and GOMODCACHE must be different directories"

out="$(realpath -m -- "${CD_BACKEND_OUT:-$base/out}")"
case "$out" in "$temp"/?*) ;; *) fail "CD_BACKEND_OUT=$out is outside RUNNER_TEMP" ;; esac
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

mkdir -p "$base/dl" "$base/toolchain" "$base/gopath" "$base/tmp" "$base/xdg-cache" "$base/xdg-config" "$out"
tarball="$base/dl/go${go_version}.linux-amd64.tar.gz"
curl -fsSL --retry 3 --retry-delay 2 --max-time 300 -o "$tarball" "https://go.dev/dl/go${go_version}.linux-amd64.tar.gz"
echo "${go_sha256}  ${tarball}" | sha256sum -c - >/dev/null || fail "Go ${go_version} tarball checksum mismatch"
tar -xzf "$tarball" -C "$base/toolchain"
rm -f "$tarball"

export PATH="$base/toolchain/go/bin:$PATH"
export GOROOT="$base/toolchain/go" GOTOOLCHAIN=local GOPATH="$base/gopath" GOTMPDIR="$base/tmp"
export XDG_CACHE_HOME="$base/xdg-cache" XDG_CONFIG_HOME="$base/xdg-config"
# One go build at a time, at most 3 compiles in it. -buildvcs=true stamps each
# binary with the checkout's vcs.revision and vcs.modified. -modcacherw keeps
# the module cache removable.
export GOFLAGS="-p=3 -buildvcs=true -modcacherw"
go version | grep -F "go${go_version} " >/dev/null || fail "toolchain is not Go ${go_version}: $(go version)"

started=$SECONDS
cd "$root/server" && go mod download

(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/migrate" ./cmd/migrate)
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/maintenance" ./cmd/maintenance)
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/backfill_task_usage_hourly" ./cmd/backfill_task_usage_hourly)
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/backfill_codex_usage_cache" ./cmd/backfill_codex_usage_cache)
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
