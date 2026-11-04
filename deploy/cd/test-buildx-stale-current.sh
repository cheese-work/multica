#!/usr/bin/env bash
# CHE-3594: an existing named builder plus a stale buildx/current must not make an unbound
# `docker buildx build --provenance --sbom` fall back to the docker driver. Skips without docker.
set -euo pipefail

docker buildx version >/dev/null 2>&1 || { echo "SKIP: docker buildx unavailable"; exit 0; }

work="$(mktemp -d)"
export DOCKER_CONFIG="$work/docker"
name="t-stale-current-$$"
cleanup() { docker buildx rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

mkdir -p "$work/ctx"
printf 'FROM scratch\nCOPY f /f\n' >"$work/ctx/Dockerfile"
echo x >"$work/ctx/f"

docker buildx create --name "$name" --driver docker-container >/dev/null
# What a previous job left behind: current names a builder that no longer exists.
printf '{"Key":"unix:///var/run/docker.sock","Name":"builder-gone","Global":false}' >"$DOCKER_CONFIG/buildx/current"

build() { docker buildx build --provenance=true --sbom=true --output type=oci,dest=/dev/null "$work/ctx" 2>&1; }

if unbound="$(build)"; then
  echo "FAIL: unbound build succeeded; stale-pointer scenario not reproduced"; exit 1
fi
grep -q 'Attestation is not supported for the docker driver' <<<"$unbound" || { echo "FAIL: unexpected error: $unbound"; exit 1; }
echo "red: unbound build fails with docker-driver attestation error"

BUILDX_BUILDER="$name" build >/dev/null || { echo "FAIL: build bound via BUILDX_BUILDER failed"; exit 1; }
echo "green: build bound via BUILDX_BUILDER succeeds"
