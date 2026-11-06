#!/usr/bin/env bash
# CHE-3594: an existing named builder plus a stale buildx/current must not make an unbound
# `docker buildx build --provenance --sbom` fall back to the docker driver. Skips without docker.
# The build env (BUILDX_BUILDER or none) comes from the workflow under test, so the result
# flips with the workflow: usage `test-buildx-stale-current.sh [workflow.yml]`.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="${1:-$root/.github/workflows/cd-qualification.yml}"

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

# Mirrors the workflow: the setup step's output is the named builder; a build step either binds
# BUILDX_BUILDER to it or leaves docker to follow buildx/current.
bound="$(python3 - "$workflow" <<'PY'
import sys, yaml
steps = yaml.safe_load(open(sys.argv[1]))['jobs']['build-images']['steps']
builds = [s for s in steps if 'docker buildx build' in s.get('run', '')]
assert builds, 'no docker buildx build step'
ok = all(s.get('env', {}).get('BUILDX_BUILDER') == '${{ steps.buildx.outputs.name }}' for s in builds)
print('yes' if ok else 'no')
PY
)"
echo "workflow: $workflow (builds bound to named builder: $bound)"

build() { docker buildx build --provenance=true --sbom=true --output type=oci,dest=/dev/null "$work/ctx" 2>&1; }

if [ "$bound" = yes ]; then export BUILDX_BUILDER="$name"; fi

if out="$(build)"; then
  echo "PASS: build succeeds with the workflow's builder binding"
else
  grep -q 'Attestation is not supported for the docker driver' <<<"$out" || { echo "FAIL: unexpected error: $out"; exit 1; }
  echo "FAIL: stale buildx/current, unbound build fell back to the docker driver: Attestation is not supported for the docker driver"
  exit 1
fi
