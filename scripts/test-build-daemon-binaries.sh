#!/bin/sh
# Builds one platform and checks manifest.json against the files on disk.
set -eu
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT
commit=0123456789abcdef0123456789abcdef01234567
platform="$(cd "$root/server" && go env GOOS)/$(cd "$root/server" && go env GOARCH)"
key="$(echo "$platform" | tr / -)"

COMMIT="$commit" VERSION=9.9.9 BUILD=test DAEMON_PLATFORMS="$platform" "$root/scripts/build-daemon-binaries.sh" "$out"

grep -q "\"commit\":\"$commit\"" "$out/manifest.json" || { echo "FAIL: manifest commit"; exit 1; }
grep -q '"version":"9.9.9-test"' "$out/manifest.json" || { echo "FAIL: manifest version"; exit 1; }
sum="$(sha256sum "$out/multica-$key" | cut -d' ' -f1)"
grep -q "\"$key\":{\"sha256\":\"$sum\"" "$out/manifest.json" || { echo "FAIL: manifest sha256"; exit 1; }
"$out/multica-$key" version 2>&1 | grep -q "$commit" || { echo "FAIL: binary not stamped with commit"; "$out/multica-$key" version; exit 1; }
echo "PASS: $key"
