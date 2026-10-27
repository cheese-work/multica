#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
source deploy/cd/deploy-lib.sh
scratch="$(mktemp -d)"
trap 'ghcr_logout; rm -f "$scratch/docker" "$scratch/calls"; rmdir "$scratch"' EXIT
cat > "$scratch/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1:$DOCKER_CONFIG" >> "$GHCR_CALLS"
case "$1" in
  login)
    test "$DOCKER_CONFIG" != "$ORIGINAL_DOCKER_CONFIG"
    test "$(stat -c %a "$DOCKER_CONFIG")" = 700
    node -e 'const fs=require("node:fs"); const config=JSON.parse(fs.readFileSync(process.env.DOCKER_CONFIG+"/config.json")); if(config.auths || config.credHelpers["ghcr.io"]!=="multica-ghcr") process.exit(1)'
    test "$(printf '%s' ghcr.io | docker-credential-multica-ghcr get)" = '{"Username":"token","Secret":"fixture-token"}'
    printf '%s' '{"ServerURL":"ghcr.io","Username":"token","Secret":"fixture-token"}' | docker-credential-multica-ghcr store
    ! rg -q fixture-token "$DOCKER_CONFIG"
    test "${FAIL_LOGIN:-0}" = 0
    ;;
  logout) printf '%s' ghcr.io | docker-credential-multica-ghcr erase ;;
  *) exit 1 ;;
esac
DOCKER
chmod 700 "$scratch/docker"
export ORIGINAL_DOCKER_CONFIG="$scratch/original"
export DOCKER_CONFIG="$ORIGINAL_DOCKER_CONFIG"
export PATH="$scratch:$PATH"
export GHCR_PULL_TOKEN=fixture-token
export GHCR_CALLS="$scratch/calls"
original_path="$PATH"
ghcr_logout
test ! -e "$GHCR_CALLS"
ghcr_login
helper_dir="$DOCKER_CONFIG"
ghcr_logout
test "$(wc -l < "$GHCR_CALLS")" = 2
test ! -e "$helper_dir"
test "$DOCKER_CONFIG" = "$ORIGINAL_DOCKER_CONFIG"
test "$PATH" = "$original_path"
ghcr_logout
export FAIL_LOGIN=1
if ghcr_login; then exit 1; fi
helper_dir="$DOCKER_CONFIG"
ghcr_logout
test ! -e "$helper_dir"
test "$DOCKER_CONFIG" = "$ORIGINAL_DOCKER_CONFIG"
printf 'PASS: ephemeral credential helper on successful and failed login; no token on disk\n'
