#!/usr/bin/env bash
# Read-only. Prints OK or FAIL per dependency; exits 1 on any FAIL. Run before driving.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$root"
fail=0
check() { if "${@:2}" >/dev/null 2>&1; then echo "OK   $1"; else echo "FAIL $1"; fail=1; fi; }
check "host is X99" test "$(hostname)" = congvc-X99
check "node_modules installed" test -d node_modules/@playwright
check "chromium for playwright" bash -c 'ls ~/.cache/ms-playwright | grep -q "^chromium"'
j="$(make status ARGS=--json 2>/dev/null || true)"
check "environment registered" test -n "$j"
api="$(printf '%s' "$j" | node -pe 'try{JSON.parse(require("fs").readFileSync(0,"utf8")).backend_port}catch{""}')"
web="$(printf '%s' "$j" | node -pe 'try{JSON.parse(require("fs").readFileSync(0,"utf8")).frontend_port}catch{""}')"
check "api /health answers (port $api)" curl -sf --max-time 5 "http://localhost:$api/health"
check "web answers /login (port $web)" curl -sf --max-time 400 -o /dev/null "http://localhost:$web/login"
db="$(printf '%s' "$j" | node -pe 'try{JSON.parse(require("fs").readFileSync(0,"utf8")).database}catch{""}')"
check "isolated database $db (not the shared one)" bash -c "[ -n '$db' ] && [ '$db' != multica ] && docker exec multica-postgres-1 psql -U multica -d '$db' -Atc 'select 1'"
exit $fail
