#!/bin/sh
# Cross-compile the multica CLI/daemon for every platform a daemon can run on
# and write manifest.json (commit, version, sha256, size) beside the binaries.
# The server image serves these to daemons (server/internal/handler/daemon_update.go).
#
# Usage: scripts/build-daemon-binaries.sh <out-dir>
# Env:   VERSION BUILD COMMIT DATE   (same meaning as the Dockerfile build args)
#        DAEMON_PLATFORMS            space-separated os/arch list (default: all four)
set -eu

out="${1:?usage: build-daemon-binaries.sh <out-dir>}"
version="${VERSION:-dev}"
build="${BUILD:-}"
commit="${COMMIT:-unknown}"
date="${DATE:-unknown}"
platforms="${DAEMON_PLATFORMS:-linux/amd64 linux/arm64 darwin/arm64 darwin/amd64}"

full_version="${version}${build:+-$build}"
ldflags="-s -w -X main.version=${full_version} -X main.commit=${commit} -X main.date=${date}"

root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
rm -f "$out"/multica-* "$out/manifest.json"

entries=""
for platform in $platforms; do
  goos="${platform%/*}"
  goarch="${platform#*/}"
  key="${goos}-${goarch}"
  (cd "$root/server" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -ldflags "$ldflags" -o "$out/multica-$key" ./cmd/multica)
  sum="$(sha256sum "$out/multica-$key" | cut -d' ' -f1)"
  size="$(wc -c < "$out/multica-$key" | tr -d ' ')"
  entries="${entries}${entries:+,}\"${key}\":{\"sha256\":\"${sum}\",\"size\":${size}}"
done

printf '{"commit":"%s","version":"%s","binaries":{%s}}\n' "$commit" "$full_version" "$entries" > "$out/manifest.json"
