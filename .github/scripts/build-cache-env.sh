set -euo pipefail
printf '%s\n' \
  "GOCACHE=$RUNNER_TEMP/build-cache/go-build" \
  "GOPATH=$RUNNER_TEMP/build-cache/go" \
  "GOMODCACHE=$RUNNER_TEMP/build-cache/go/pkg/mod" \
  "GOFLAGS=${GOFLAGS:+$GOFLAGS }-modcacherw" \
  "XDG_CACHE_HOME=$RUNNER_TEMP/build-cache/xdg" \
  "XDG_DATA_HOME=$RUNNER_TEMP/build-cache/data" \
  "npm_config_cache=$RUNNER_TEMP/build-cache/npm" \
  "npm_config_store_dir=$RUNNER_TEMP/build-cache/pnpm" \
  "PIP_CACHE_DIR=$RUNNER_TEMP/build-cache/pip" \
  "PYTHONPYCACHEPREFIX=$RUNNER_TEMP/build-cache/pycache" \
  "UV_CACHE_DIR=$RUNNER_TEMP/build-cache/uv" \
  "BUILDX_CONFIG=$RUNNER_TEMP/build-cache/buildx" \
  >> "$GITHUB_ENV"
