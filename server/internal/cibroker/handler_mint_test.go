package cibroker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/multica-ai/multica/server/internal/cibroker/mint"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

type fakeIssuer struct {
	got mint.Request
	tok mint.Token
	err error
}

func (f *fakeIssuer) Issue(_ context.Context, req mint.Request) (mint.Token, error) {
	f.got = req
	return f.tok, f.err
}

func flagOn() *featureflag.Service {
	p := featureflag.NewStaticProvider()
	p.Set("ci_token_broker", featureflag.Rule{Default: true})
	return featureflag.NewService(p)
}

func serve(f *fixture, iss TokenIssuer, claims map[string]any, body string) *httptest.ResponseRecorder {
	h := &Handler{Flags: flagOn(), Broker: f.broker, Mint: iss}
	req := httptest.NewRequest(http.MethodPost, "/api/ci/github-token", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.sign(jwt.MapClaims(claims), f.key, jwt.SigningMethodRS256))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHandlerMintsFromVerifiedClaimsAndEntry(t *testing.T) {
	f := newFixture(t)
	iss := &fakeIssuer{tok: mint.Token{
		Value: mint.Secret("ghs_secret"), ExpiresAt: f.now.Add(time.Hour),
		Permissions: mint.Permissions{"contents": "read"},
	}}
	w := serve(f, iss, f.claims(), `{"permissions":{"contents":"read","issues":"read"}}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if !strings.Contains(w.Body.String(), "ghs_secret") {
		t.Errorf("body has no token: %s", w.Body)
	}
	g := iss.got
	if g.RepositoryID != 111 || g.Repository != testRepo || g.RunID != "9" || g.RunAttempt != "1" || g.Event != "push" {
		t.Errorf("request not from verified claims: %+v", g)
	}
	if g.Wanted["contents"] != "read" || g.Wanted["issues"] != "read" {
		t.Errorf("Wanted = %v", g.Wanted)
	}
	if g.EntryMax["contents"] != "read" || g.EntryMax["actions"] != "read" || len(g.EntryMax) != 2 {
		t.Errorf("EntryMax = %v, want the policy entry's permissions", g.EntryMax)
	}
}

func TestHandlerWritesRefusalWithRetryAfter(t *testing.T) {
	f := newFixture(t)
	iss := &fakeIssuer{err: &mint.Refusal{Status: 429, Reason: "quota_core", RetryAfter: 90 * time.Second}}
	w := serve(f, iss, f.claims(), `{"permissions":{"contents":"read"}}`)
	if w.Code != 429 || w.Header().Get("Retry-After") != "90" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("got %d %v", w.Code, w.Header())
	}
}

func TestHandlerRejectsBadBodyWithoutMinting(t *testing.T) {
	f := newFixture(t)
	for name, body := range map[string]string{
		"not json":          "nope",
		"permissions array": `{"permissions":[]}`,
		"unknown field":     `{"permissions":{},"x":1}`,
		"repository field":  `{"permissions":{"contents":"read"},"repository_id":"999"}`,
	} {
		iss := &fakeIssuer{}
		w := serve(f, iss, f.claims(), body)
		if w.Code != 400 {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
		if iss.got.RepositoryID != 0 {
			t.Errorf("%s: minted anyway", name)
		}
	}
}

func TestHandlerWithoutMinterIs501(t *testing.T) {
	f := newFixture(t)
	if w := serve(f, nil, f.claims(), `{}`); w.Code != 501 {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

func TestHandlerPolicyRefusalNeverReachesMint(t *testing.T) {
	f := newFixture(t)
	iss := &fakeIssuer{}
	c := f.claims()
	c["event_name"] = "pull_request"
	if w := serve(f, iss, c, `{"permissions":{"contents":"read"}}`); w.Code != 403 || iss.got.RepositoryID != 0 {
		t.Errorf("status %d, issuer called %v", w.Code, iss.got)
	}
}
