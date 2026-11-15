#!/usr/bin/env bash
# CHE-4877: fetch-verified.sh retries 5xx only, verifies the checksum, and reuses a cached file that still verifies.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
t="$(mktemp -d)"; trap 'kill "${srv:-0}" 2>/dev/null || true; rm -rf "$t"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

printf 'payload' > "$t/ok.bin"; sum="$(sha256sum "$t/ok.bin" | cut -d' ' -f1)"
cat > "$t/srv.py" <<'PY'
import http.server, sys
hits = {}
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        hits[self.path] = hits.get(self.path, 0) + 1
        n = hits[self.path]
        if self.path == "/stats":
            body = repr(hits).encode(); code = 200
        elif self.path == "/flaky" and n <= 2: body = b"x"; code = 504
        elif self.path == "/flaky": body = b"payload"; code = 200
        elif self.path == "/down": body = b"x"; code = 503
        elif self.path == "/missing": body = b"x"; code = 404
        else: body = b"payload"; code = 200
        self.send_response(code); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
print(s.server_port, flush=True)
s.serve_forever()
PY
python3 -I "$t/srv.py" > "$t/port" & srv=$!
for _ in $(seq 50); do [[ -s "$t/port" ]] && break; sleep 0.1; done
base="http://127.0.0.1:$(cat "$t/port")"
export FETCH_RETRY_DELAY=0
hits() { curl -fsS "$base/stats" | python3 -I -c "import sys,ast;print(ast.literal_eval(sys.stdin.read()).get('$1',0))"; }

# 1. two 504s then success: file lands, checksum passes
"$here/fetch-verified.sh" "$base/flaky" "$sum" "$t/a.bin" >/dev/null 2>&1 || fail "5xx was not retried to success"
[[ "$(cat "$t/a.bin")" == payload ]] || fail "wrong content"
[[ "$(hits /flaky)" == 3 ]] || fail "expected 3 requests to /flaky, got $(hits /flaky)"

# 2. 404 is not retried
"$here/fetch-verified.sh" "$base/missing" "$sum" "$t/b.bin" >/dev/null 2>&1 && fail "404 should fail"
[[ "$(hits /missing)" == 1 ]] || fail "404 was retried ($(hits /missing) requests)"

# 3. persistent 5xx stops after a bounded number of attempts
"$here/fetch-verified.sh" "$base/down" "$sum" "$t/c.bin" >/dev/null 2>&1 && fail "persistent 503 should fail"
[[ "$(hits /down)" == 4 ]] || fail "expected 4 bounded attempts, got $(hits /down)"

# 4. wrong checksum fails and leaves no file behind
"$here/fetch-verified.sh" "$base/ok" "$(printf 0%.0s $(seq 64))" "$t/d.bin" >/dev/null 2>&1 && fail "bad checksum accepted"
[[ ! -e "$t/d.bin" ]] || fail "unverified file left in place"

# 5. a cached file that verifies is reused with no request; a corrupt one is replaced
before="$(hits /ok)"
"$here/fetch-verified.sh" "$base/ok" "$sum" "$t/a.bin" >/dev/null 2>&1 || fail "cached file rejected"
[[ "$(hits /ok)" == "$before" ]] || fail "cache hit still hit the network"
echo corrupt > "$t/a.bin"
"$here/fetch-verified.sh" "$base/ok" "$sum" "$t/a.bin" >/dev/null 2>&1 || fail "corrupt cache not replaced"
[[ "$(cat "$t/a.bin")" == payload ]] || fail "corrupt cache kept"
echo PASS
