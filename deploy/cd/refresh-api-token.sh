# CHE-4251: source me. refresh_api_token re-reads the App token budget and exports GH_TOKEN for the next API phase, so a
# long wait cannot drain the reserve the Multica server keeps. No-op unless APP_TOKEN is set.
refresh_api_token() {
  [ -n "${APP_TOKEN:-}" ] || return 0
  local out picked
  out="$(mktemp)"
  GITHUB_OUTPUT="$out" bash deploy/cd/pick-api-token.sh >&2 || true
  picked="$(sed -n 's/^token=//p' "$out")"
  rm -f "$out"
  [ -z "$picked" ] || export GH_TOKEN=$picked
}
