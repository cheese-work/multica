package cardref

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const token = "0123456789abcdef0123456789abcdef"

// The three calls doop's MulticaPort makes. The JSON is what tests/support/pinHarness.ts must return.
func testHandler(t *testing.T, p *pgxpool.Pool) {
	h := NewHandler(p, token)
	do := func(method, path, auth, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	bearer := "Bearer " + token
	ctx := context.Background()
	seed(t, p, "h-open", Open)

	// ordered: "abort_ref voids open" changes the row the earlier cases read
	for _, tc := range []struct {
		name, method, path, auth, body string
		code                           int
		out                            string
	}{
		{"status of an open card", "GET", "/api/service/card-refs/h-open/status", bearer, "", 200, `{"status":"open"}`},
		{"status of a missing card", "GET", "/api/service/card-refs/h-none/status", bearer, "", 200, `{"status":"missing"}`},
		{"abort_if_pending leaves open", "POST", "/api/service/card-refs/h-open/abort-if-pending", bearer, "", 200, `{"status":"open","notAborted":true}`},
		{"abort_if_pending on missing", "POST", "/api/service/card-refs/h-ghost/abort-if-pending", bearer, "", 200, `{"status":"void","notAborted":false}`},
		{"abort_ref voids open", "POST", "/api/service/card-refs/h-open/abort", bearer, `{"reason":"force_release"}`, 200, `{"status":"void"}`},
		{"no token", "GET", "/api/service/card-refs/h-open/status", "", "", 401, ""},
		{"wrong token", "GET", "/api/service/card-refs/h-open/status", "Bearer nope", "", 401, ""},
		{"bad id", "GET", "/api/service/card-refs/a.b/status", bearer, "", 400, ""},
		{"abort without a reason", "POST", "/api/service/card-refs/h-x/abort", bearer, `{}`, 400, ""},
		{"abort with bad json", "POST", "/api/service/card-refs/h-x/abort", bearer, `{`, 400, ""},
		{"wrong method", "POST", "/api/service/card-refs/h-open/status", bearer, "", 405, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := do(tc.method, tc.path, tc.auth, tc.body)
			if code != tc.code || (tc.out != "" && out != tc.out) {
				t.Fatalf("got %d %q, want %d %q", code, out, tc.code, tc.out)
			}
		})
	}
	expect(t, "after abort_ref", statusOf(t, p, "h-open"), Void)
	_ = ctx

	// a token that is too short means the channel is not configured: every call is refused
	weak := NewHandler(p, "short")
	req := httptest.NewRequest("GET", "/api/service/card-refs/h-open/status", nil)
	req.Header.Set("Authorization", "Bearer short")
	rec := httptest.NewRecorder()
	weak.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured handler answered %d", rec.Code)
	}
}
