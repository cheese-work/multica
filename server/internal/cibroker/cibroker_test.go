package cibroker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

const (
	testRepo     = "cheese-work/multica"
	testWorkflow = ".github/workflows/cd-deploy.yml"
	testPolicy   = `
entries:
  - repository: cheese-work/multica
    repository_id: "111"
    repository_owner_id: "222"
    workflow: .github/workflows/cd-deploy.yml
    events: [push, workflow_run, workflow_dispatch]
    ref: refs/heads/main
    runner_environment: self-hosted
    environments: ["", c00-runtime]
    workflow_run_guard: "cd-deploy.yml:42-44"
    permissions: {contents: read, actions: read}
`
)

type staticKeys map[string]*rsa.PublicKey

func (s staticKeys) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if k, ok := s[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown kid")
}

type fixture struct {
	t      *testing.T
	key    *rsa.PrivateKey
	now    time.Time
	broker *Broker
	jti    int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := LoadPolicy([]byte(testPolicy))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	f := &fixture{t: t, key: key, now: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	v := NewVerifier(staticKeys{"k1": &key.PublicKey})
	v.Now = func() time.Time { return f.now }
	f.broker = &Broker{Verifier: v, Policy: pol}
	return f
}

// claims returns a valid ordinary `push` claim set; tests mutate it.
func (f *fixture) claims() jwt.MapClaims {
	f.jti++
	wf := testRepo + "/" + testWorkflow + "@" + MainRef
	return jwt.MapClaims{
		"iss": Issuer, "aud": Audience, "sub": "repo:" + testRepo,
		"iat": f.now.Unix(), "nbf": f.now.Unix(), "exp": f.now.Add(10 * time.Minute).Unix(),
		"jti": "jti-" + string(rune('a'+f.jti)), "repository": testRepo,
		"repository_id": "111", "repository_owner_id": "222",
		"ref": MainRef, "event_name": "push",
		"workflow_ref": wf, "job_workflow_ref": wf,
		"runner_environment": "self-hosted", "run_id": "9", "run_attempt": "1",
	}
}

func (f *fixture) sign(c jwt.MapClaims, key *rsa.PrivateKey, method jwt.SigningMethod) string {
	f.t.Helper()
	tok := jwt.NewWithClaims(method, c)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(key)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func status(err error) int {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Status
	}
	return 0
}

func TestAcceptsOrdinaryEvents(t *testing.T) {
	for _, event := range []string{"push", "workflow_run", "workflow_dispatch"} {
		f := newFixture(t)
		c := f.claims()
		c["event_name"] = event
		entry, got, err := f.broker.Authorize(context.Background(), f.sign(c, f.key, jwt.SigningMethodRS256))
		if err != nil || entry == nil || got == nil {
			t.Fatalf("%s: want accepted, got entry=%v err=%v", event, entry, err)
		}
		if got.RepositoryID != "111" || got.EventName != event {
			t.Errorf("%s: claims = %+v", event, got)
		}
	}
}

func TestRefusals(t *testing.T) {
	other := testRepo + "/.github/workflows/other.yml@" + MainRef
	cases := []struct {
		name   string
		mutate func(c jwt.MapClaims)
		want   int
	}{
		{"wrong iss", func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }, 401},
		{"wrong aud", func(c jwt.MapClaims) { c["aud"] = "sts.amazonaws.com" }, 401},
		{"old iat", func(c jwt.MapClaims) { c["iat"] = time.Date(2026, 11, 12, 5, 54, 0, 0, time.UTC).Unix() }, 401},
		{"future iat", func(c jwt.MapClaims) { c["iat"] = time.Date(2026, 11, 12, 6, 10, 0, 0, time.UTC).Unix() }, 401},
		{"expired", func(c jwt.MapClaims) { c["exp"] = time.Date(2026, 11, 12, 5, 58, 0, 0, time.UTC).Unix() }, 401},
		{"missing exp", func(c jwt.MapClaims) { delete(c, "exp") }, 401},
		{"missing jti", func(c jwt.MapClaims) { delete(c, "jti") }, 401},
		{"missing repository_id", func(c jwt.MapClaims) { delete(c, "repository_id") }, 401},
		{"missing workflow_ref", func(c jwt.MapClaims) { delete(c, "workflow_ref") }, 401},
		{"non-string claim", func(c jwt.MapClaims) { c["repository_id"] = 111 }, 401},
		{"pull_request", func(c jwt.MapClaims) { c["event_name"] = "pull_request" }, 403},
		{"pull_request_target on main", func(c jwt.MapClaims) { c["event_name"] = "pull_request_target" }, 403},
		{"issue_comment", func(c jwt.MapClaims) { c["event_name"] = "issue_comment" }, 403},
		{"merge_group", func(c jwt.MapClaims) { c["event_name"] = "merge_group" }, 403},
		{"wrong repo owner id", func(c jwt.MapClaims) { c["repository_owner_id"] = "999" }, 403},
		{"wrong repo id", func(c jwt.MapClaims) { c["repository_id"] = "112" }, 403},
		{"wrong repository name", func(c jwt.MapClaims) { c["repository"] = "cheese-work/other" }, 403},
		{"wrong workflow path", func(c jwt.MapClaims) { c["workflow_ref"] = other; c["job_workflow_ref"] = other }, 403},
		{"dispatch on non-main ref", func(c jwt.MapClaims) {
			wf := testRepo + "/" + testWorkflow + "@refs/heads/feature"
			c["event_name"], c["ref"], c["workflow_ref"], c["job_workflow_ref"] = "workflow_dispatch", "refs/heads/feature", wf, wf
		}, 403},
		{"workflow_ref not on main", func(c jwt.MapClaims) {
			wf := testRepo + "/" + testWorkflow + "@refs/heads/feature"
			c["workflow_ref"], c["job_workflow_ref"] = wf, wf
		}, 403},
		{"reusable call: job_workflow_ref differs", func(c jwt.MapClaims) {
			c["job_workflow_ref"] = "cheese-work/shared/.github/workflows/reuse.yml@" + MainRef
		}, 403},
		{"reusable call: same path, other sha", func(c jwt.MapClaims) {
			c["job_workflow_ref"] = testRepo + "/" + testWorkflow + "@refs/heads/other"
		}, 403},
		{"wrong runner environment", func(c jwt.MapClaims) { c["runner_environment"] = "github-hosted" }, 403},
		{"environment not allowed", func(c jwt.MapClaims) { c["environment"] = "prod" }, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c := f.claims()
			tc.mutate(c)
			_, _, err := f.broker.Authorize(context.Background(), f.sign(c, f.key, jwt.SigningMethodRS256))
			if got := status(err); got != tc.want {
				t.Fatalf("status = %d (err %v), want %d", got, err, tc.want)
			}
		})
	}
}

func TestSignatureAndAlgorithm(t *testing.T) {
	f := newFixture(t)
	forger, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, _, err := f.broker.Authorize(context.Background(), f.sign(f.claims(), forger, jwt.SigningMethodRS256)); status(err) != 401 {
		t.Errorf("forged signature: err = %v, want 401", err)
	}
	if _, _, err := f.broker.Authorize(context.Background(), f.sign(f.claims(), f.key, jwt.SigningMethodRS384)); status(err) != 401 {
		t.Errorf("RS384: err = %v, want 401", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, f.claims())
	tok.Header["kid"] = "k1"
	hs, _ := tok.SignedString([]byte("x"))
	if _, _, err := f.broker.Authorize(context.Background(), hs); status(err) != 401 {
		t.Errorf("HS256: err = %v, want 401", err)
	}
	if _, _, err := f.broker.Authorize(context.Background(), "not-a-jwt"); status(err) != 401 {
		t.Errorf("garbage: err = %v, want 401", err)
	}
	c := f.claims()
	tk := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tk.Header["kid"] = "unknown"
	s, _ := tk.SignedString(f.key)
	if _, _, err := f.broker.Authorize(context.Background(), s); status(err) != 401 {
		t.Errorf("unknown kid: err = %v, want 401", err)
	}
}

func TestReplayedJTI(t *testing.T) {
	f := newFixture(t)
	tok := f.sign(f.claims(), f.key, jwt.SigningMethodRS256)
	if _, _, err := f.broker.Authorize(context.Background(), tok); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, _, err := f.broker.Authorize(context.Background(), tok); status(err) != 401 {
		t.Fatalf("replay: err = %v, want 401", err)
	}
	// Once exp has passed the jti is forgotten, but the token is expired anyway.
	f.now = f.now.Add(11 * time.Minute)
	if _, _, err := f.broker.Authorize(context.Background(), tok); status(err) != 401 {
		t.Fatalf("after exp: err = %v, want 401", err)
	}
}

// A token stays acceptable until exp+skew, so its jti must be remembered that long.
func TestReplayedJTIInsideLeeway(t *testing.T) {
	f := newFixture(t)
	c := f.claims()
	c["exp"] = f.now.Add(2 * time.Minute).Unix()
	tok := f.sign(c, f.key, jwt.SigningMethodRS256)
	if _, _, err := f.broker.Authorize(context.Background(), tok); err != nil {
		t.Fatalf("first use: %v", err)
	}
	f.now = f.now.Add(2*time.Minute + 30*time.Second) // past exp, inside the 60 s leeway
	if _, _, err := f.broker.Authorize(context.Background(), tok); status(err) != 401 {
		t.Fatalf("replay inside leeway: err = %v, want 401", err)
	}
	// Five-minute lifetime, replayed exactly at exp.
	f.now = f.now.Add(time.Hour)
	c = f.claims()
	c["exp"] = f.now.Add(5 * time.Minute).Unix()
	tok = f.sign(c, f.key, jwt.SigningMethodRS256)
	if _, _, err := f.broker.Authorize(context.Background(), tok); err != nil {
		t.Fatalf("first use: %v", err)
	}
	f.now = f.now.Add(5 * time.Minute)
	if _, _, err := f.broker.Authorize(context.Background(), tok); status(err) != 401 {
		t.Fatalf("replay at exp: err = %v, want 401", err)
	}
}

func TestLoadPolicyRejectsUnsafeEntries(t *testing.T) {
	base := func(mut func(string) string) string { return mut(testPolicy) }
	for name, body := range map[string]string{
		"non-main ref":       base(func(s string) string { return strings.Replace(s, "ref: refs/heads/main", "ref: refs/heads/dev", 1) }),
		"pull_request event": base(func(s string) string { return strings.Replace(s, "[push,", "[pull_request, push,", 1) }),
		"merge_group event":  base(func(s string) string { return strings.Replace(s, "[push,", "[merge_group, push,", 1) }),
		"workflow_run no guard": base(func(s string) string {
			return strings.Replace(s, `    workflow_run_guard: "cd-deploy.yml:42-44"`+"\n", "", 1)
		}),
		"write permission":        base(func(s string) string { return strings.Replace(s, "contents: read", "contents: write", 1) }),
		"permission over ceiling": base(func(s string) string { return strings.Replace(s, "actions: read", "administration: read", 1) }),
		"missing repository_id":   base(func(s string) string { return strings.Replace(s, `    repository_id: "111"`+"\n", "", 1) }),
		"no entries":              "entries: []",
		"unknown field":           testPolicy + "    installation_id: 5\n",
	} {
		if _, err := LoadPolicy([]byte(body)); err == nil {
			t.Errorf("%s: LoadPolicy accepted it", name)
		}
	}
}

func TestHandler(t *testing.T) {
	f := newFixture(t)
	onProv := featureflag.NewStaticProvider()
	onProv.Set("ci_token_broker", featureflag.Rule{Default: true})
	on := featureflag.NewService(onProv)
	off := featureflag.NewService(featureflag.NewStaticProvider())
	do := func(flags *featureflag.Service, auth, body string) int {
		h := &Handler{Flags: flags, Broker: f.broker}
		req := httptest.NewRequest(http.MethodPost, "/api/ci/github-token", strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	tok := f.sign(f.claims(), f.key, jwt.SigningMethodRS256)
	if got := do(nil, "Bearer "+tok, "{}"); got != 404 {
		t.Errorf("nil flags: %d, want 404", got)
	}
	if got := do(off, "Bearer "+tok, "{}"); got != 404 {
		t.Errorf("flag off: %d, want 404", got)
	}
	if got := do(on, "Bearer "+tok, strings.Repeat("x", 4097)); got != 413 {
		t.Errorf("big body: %d, want 413", got)
	}
	if got := do(on, "", "{}"); got != 401 {
		t.Errorf("no auth: %d, want 401", got)
	}
	if got := do(on, "Bearer junk", "{}"); got != 401 {
		t.Errorf("bad jwt: %d, want 401", got)
	}
	// Authorized, but S1 has no issuance yet: 501, never a token.
	if got := do(on, "Bearer "+tok, "{}"); got != 501 {
		t.Errorf("authorized: %d, want 501", got)
	}
	pr := f.claims()
	pr["event_name"] = "pull_request"
	if got := do(on, "Bearer "+f.sign(pr, f.key, jwt.SigningMethodRS256), "{}"); got != 403 {
		t.Errorf("pull_request: %d, want 403", got)
	}
}

func TestJWKSKeys(t *testing.T) {
	f := newFixture(t)
	hits, fail := 0, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if fail {
			http.Error(w, "down", 500)
			return
		}
		pub := f.key.PublicKey
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1","n":"` +
			base64.RawURLEncoding.EncodeToString(pub.N.Bytes()) + `","e":"AQAB"}]}`))
	}))
	defer srv.Close()
	keys := &JWKSKeys{URL: srv.URL, Now: func() time.Time { return f.now }}
	f.broker.Verifier = NewVerifier(keys)
	f.broker.Verifier.Now = func() time.Time { return f.now }

	if _, _, err := f.broker.Authorize(context.Background(), f.sign(f.claims(), f.key, jwt.SigningMethodRS256)); err != nil {
		t.Fatalf("valid token via JWKS: %v", err)
	}
	tk := jwt.NewWithClaims(jwt.SigningMethodRS256, f.claims())
	tk.Header["kid"] = "other"
	s, _ := tk.SignedString(f.key)
	if _, _, err := f.broker.Authorize(context.Background(), s); status(err) != 401 {
		t.Errorf("unknown kid: %v, want 401", err)
	}
	if _, _, err := f.broker.Authorize(context.Background(), s); status(err) != 401 || hits != 1 {
		t.Errorf("unknown kid retry: %v hits=%d, want 401 and 1 hit (no refetch inside MinRefresh)", err, hits)
	}
	// JWKS outage with an unseen kid is a 503, not an auth failure.
	f.now = f.now.Add(2 * time.Minute)
	fail = true
	if _, _, err := f.broker.Authorize(context.Background(), s); status(err) != 503 {
		t.Errorf("outage: %v, want 503", err)
	}
}

// jwksServer serves k1 from f.key while present is true; it counts requests.
func jwksServer(f *fixture, hits *int, present, fail *bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if *fail {
			http.Error(w, "down", 500)
			return
		}
		if !*present {
			_, _ = w.Write([]byte(`{"keys":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1","n":"` +
			base64.RawURLEncoding.EncodeToString(f.key.PublicKey.N.Bytes()) + `","e":"AQAB"}]}`))
	}))
}

func TestJWKSCachedKeyExpires(t *testing.T) {
	f := newFixture(t)
	hits, present, fail := 0, true, false
	srv := jwksServer(f, &hits, &present, &fail)
	defer srv.Close()
	f.broker.Verifier = NewVerifier(&JWKSKeys{URL: srv.URL, Now: func() time.Time { return f.now }})
	f.broker.Verifier.Now = func() time.Time { return f.now }

	if _, _, err := f.broker.Authorize(context.Background(), f.sign(f.claims(), f.key, jwt.SigningMethodRS256)); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	// GitHub withdraws the key. Inside the max age the cached key still works.
	present = false
	f.now = f.now.Add(30 * time.Minute)
	if _, _, err := f.broker.Authorize(context.Background(), f.sign(f.claims(), f.key, jwt.SigningMethodRS256)); err != nil {
		t.Fatalf("inside max age: %v", err)
	}
	// Past the max age the next request refetches and the withdrawn key is refused.
	f.now = f.now.Add(31 * time.Minute)
	if _, _, err := f.broker.Authorize(context.Background(), f.sign(f.claims(), f.key, jwt.SigningMethodRS256)); status(err) != 401 {
		t.Fatalf("withdrawn key after max age: err = %v, want 401", err)
	}
}

func TestJWKSFailedFetchIsThrottled(t *testing.T) {
	f := newFixture(t)
	hits, present, fail := 0, true, true
	srv := jwksServer(f, &hits, &present, &fail)
	defer srv.Close()
	f.broker.Verifier = NewVerifier(&JWKSKeys{URL: srv.URL, Now: func() time.Time { return f.now }})
	f.broker.Verifier.Now = func() time.Time { return f.now }

	for i := 0; i < 3; i++ {
		if _, _, err := f.broker.Authorize(context.Background(), f.sign(f.claims(), f.key, jwt.SigningMethodRS256)); status(err) != 503 {
			t.Fatalf("request %d during outage: err = %v, want 503", i, err)
		}
	}
	if hits != 1 {
		t.Fatalf("outbound fetches during outage = %d, want 1 (throttled by MinRefresh)", hits)
	}
	// After MinRefresh a new attempt is allowed, and recovery works.
	f.now = f.now.Add(2 * time.Minute)
	fail = false
	if _, _, err := f.broker.Authorize(context.Background(), f.sign(f.claims(), f.key, jwt.SigningMethodRS256)); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if hits != 2 {
		t.Fatalf("fetches after recovery = %d, want 2", hits)
	}
}
