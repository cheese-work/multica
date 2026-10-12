#!/usr/bin/env bash
set -euo pipefail

# ab-health-check.sh (CHE-773): a 502/refused on any public listener or the
# router-selected upstream must alarm, and a healthy window must pass.
# curl is mocked per port; the router generation is a real active.json.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir -p "$work_dir/bin" "$work_dir/router"

cat >"$work_dir/bin/curl" <<'MOCK'
#!/usr/bin/env bash
port="$(printf '%s' "${*: -1}" | sed -E 's#^.*127\.0\.0\.1:([0-9]+).*#\1#')"
code=200
[ -f "$AB_TEST_DIR/code-$port" ] && code="$(cat "$AB_TEST_DIR/code-$port")"
printf '%s' "$code"
MOCK
printf '#!/usr/bin/env bash\nexit 0\n' >"$work_dir/bin/sleep"
chmod +x "$work_dir/bin/"*
export PATH="$work_dir/bin:$PATH" AB_TEST_DIR="$work_dir"
printf '{"colour":"blue","backend_port":18091,"frontend_port":13001}\n' >"$work_dir/router/active.json"

run() { bash deploy/cd/ab-health-check.sh --router-state-dir "$work_dir/router" --window 30 --interval 10 2>&1; }

output="$(run)" || { echo "healthy: want exit 0" >&2; echo "$output" >&2; exit 1; }
[[ "$output" == *"A/B health ok: blue served for 30s"* ]] || { echo "healthy: $output" >&2; exit 1; }

for case in "18091:router-selected upstream backend-blue on :18091" "8081:public API" "3000:public web"; do
  port="${case%%:*}" want="${case#*:}"
  rm -f "$work_dir"/code-*
  echo 502 >"$work_dir/code-$port"
  set +e; output="$(run)"; status=$?; set -e
  [ "$status" -eq 1 ] || { echo "502 on :$port: exit $status, want 1" >&2; exit 1; }
  [[ "$output" == *"::error title=Multica A/B health::$want"* ]] || { echo "502 on :$port: $output" >&2; exit 1; }
  [[ "$output" == *"A/B outage runbook"* ]] || { echo "502 on :$port: no runbook pointer" >&2; exit 1; }
done

rm -f "$work_dir/router/active.json" "$work_dir"/code-*
set +e; output="$(run)"; status=$?; set -e
[ "$status" -eq 1 ] && [[ "$output" == *"no readable active generation"* ]] || { echo "missing generation: $output" >&2; exit 1; }

echo "ab-health-check.sh fixtures passed"
