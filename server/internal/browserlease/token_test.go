package browserlease

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func seed(b byte) string {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = b
	}
	return base64.StdEncoding.EncodeToString(s)
}

func str(s string) *string { return &s }

const tNow = int64(1_800_000_000)

// claims is what W3-3's Service hands the signer for a C00 S2 lease.
func claims() Claims {
	return Claims{
		Aud: "steel-gateway/c00", Sub: "agent-1", LeaseID: "lease-1", Epoch: 3, TaskID: "task-1",
		IssueID: "issue-1", DecisionID: "dec-1", UserID: "user-1", Profile: "figma", Class: "S2",
		Instance: "c00-1", Iat: tNow, Exp: tNow + int64(TokenTTL.Seconds()), JTI: "0123456789abcdef0123456789abcdef",
	}
}

func s0Claims() Claims {
	c := claims()
	c.Aud, c.Class, c.DecisionID, c.Instance = "steel-gateway/x99", "S0", "", "x99-1"
	return c
}

type jwk struct{ Kty, Crv, X, Kid, Use, Alg string }

func fetchJWKS(t *testing.T, kr *Keyring) (map[string]ed25519.PublicKey, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	kr.JWKSHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, JWKSPath, nil))
	var body struct{ Keys []jwk }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("jwks body %q: %v", rec.Body.String(), err)
	}
	out := map[string]ed25519.PublicKey{}
	for _, k := range body.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Use != "sig" || k.Alg != "EdDSA" {
			t.Fatalf("bad jwk %+v", k)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			t.Fatalf("bad x %q: %v", k.X, err)
		}
		out[k.Kid] = ed25519.PublicKey(x)
	}
	return out, rec
}

func verify(t *testing.T, tok string, keys map[string]ed25519.PublicKey, now time.Time) (jwt.MapClaims, *jwt.Token) {
	t.Helper()
	return verifyAud(t, tok, keys, now, "steel-gateway/c00")
}

// verifyAud mimics a gateway verifier: it accepts only the literal audience it is given.
func verifyAud(t *testing.T, tok string, keys map[string]ed25519.PublicKey, now time.Time, aud string) (jwt.MapClaims, *jwt.Token) {
	t.Helper()
	mc := jwt.MapClaims{}
	parsed, err := jwt.NewParser(
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(Issuer), jwt.WithAudience(aud),
		jwt.WithExpirationRequired(), jwt.WithTimeFunc(func() time.Time { return now }),
	).ParseWithClaims(tok, mc, func(tk *jwt.Token) (any, error) {
		k, ok := keys[tk.Header["kid"].(string)]
		if !ok {
			t.Fatalf("unknown kid %v", tk.Header["kid"])
		}
		return k, nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return mc, parsed
}

func sign(t *testing.T, kr *Keyring, c Claims) string {
	t.Helper()
	tok, err := kr.Sign(c)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

func TestSignClaimsAndJWKS(t *testing.T) {
	kr, err := ParseKeyring("k1:" + seed(1))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(tNow, 0)
	c := claims()
	tok := sign(t, kr, c)
	keys, rec := fetchJWKS(t, kr)
	mc, parsed := verify(t, tok, keys, now.Add(time.Minute))
	if parsed.Header["kid"] != "k1" || parsed.Header["alg"] != "EdDSA" {
		t.Fatalf("header %v", parsed.Header)
	}
	want := map[string]any{
		"iss": Issuer, "aud": "steel-gateway/c00", "sub": "agent-1", "lease_id": "lease-1", "epoch": float64(3),
		"task_id": "task-1", "issue_id": "issue-1", "decision_id": "dec-1", "user_id": "user-1",
		"profile": "figma", "class": "S2", "instance": "c00-1", "jti": c.JTI,
		"iat": float64(tNow), "exp": float64(tNow + 900),
	}
	if len(mc) != len(want) {
		t.Fatalf("claims %v", mc)
	}
	for k, v := range want {
		if mc[k] != v {
			t.Errorf("claim %s = %v, want %v", k, mc[k], v)
		}
	}
	if rec.Header().Get("Content-Type") != "application/jwk-set+json" {
		t.Errorf("content-type %q", rec.Header().Get("Content-Type"))
	}
	// Expired after 15 minutes.
	if _, err := jwt.NewParser(jwt.WithTimeFunc(func() time.Time { return now.Add(16 * time.Minute) })).
		Parse(tok, func(*jwt.Token) (any, error) { return keys["k1"], nil }); err == nil {
		t.Fatal("token valid after TTL")
	}
}

// The gateway on each host accepts only its own audience (shared-browser steel-gateway/verify.go).
func TestAudienceIsHostSpecific(t *testing.T) {
	kr, _ := ParseKeyring("k1:" + seed(1))
	keys, _ := fetchJWKS(t, kr)
	now := time.Unix(tNow, 0)
	accepts := func(tok, aud string) bool {
		_, err := jwt.NewParser(jwt.WithAudience(aud), jwt.WithTimeFunc(func() time.Time { return now })).
			Parse(tok, func(*jwt.Token) (any, error) { return keys["k1"], nil })
		return err == nil
	}
	for _, tc := range []struct {
		c          Claims
		aud, other string
	}{
		{claims(), "steel-gateway/c00", "steel-gateway/x99"},
		{s0Claims(), "steel-gateway/x99", "steel-gateway/c00"},
	} {
		tok := sign(t, kr, tc.c)
		if !accepts(tok, tc.aud) {
			t.Errorf("%s token rejected by its own verifier", tc.aud)
		}
		if accepts(tok, tc.other) {
			t.Errorf("%s token accepted by %s verifier", tc.aud, tc.other)
		}
		if accepts(tok, "steel-gateway") {
			t.Errorf("%s token accepted with the generic audience", tc.aud)
		}
	}
}

func TestDecisionNullOnlyForS0(t *testing.T) {
	kr, _ := ParseKeyring("k1:" + seed(1))
	keys, _ := fetchJWKS(t, kr)
	tok, err := kr.Sign(s0Claims())
	if err != nil {
		t.Fatalf("S0 null decision: %v", err)
	}
	mc, _ := verifyAud(t, tok, keys, time.Unix(tNow, 0), "steel-gateway/x99")
	if v, ok := mc["decision_id"]; !ok || v != nil {
		t.Fatalf("decision_id must be present and null, got %v (present=%v)", v, ok)
	}
	for _, class := range []string{"S2", "H"} {
		c := claims()
		c.Class, c.DecisionID = class, ""
		if _, err := kr.Sign(c); err == nil {
			t.Errorf("class %s signed without decision", class)
		}
	}
}

func TestSignRejectsBadClaims(t *testing.T) {
	kr, _ := ParseKeyring("k1:" + seed(1))
	cases := map[string]func(*Claims){
		"S1 class":       func(c *Claims) { c.Class = "S1" },
		"unknown class":  func(c *Claims) { c.Class = "x" },
		"generic aud":    func(c *Claims) { c.Aud = "steel-gateway" },
		"empty aud":      func(c *Claims) { c.Aud = "" },
		"other host aud": func(c *Claims) { c.Aud = "steel-gateway/mbp" },
		"empty sub":      func(c *Claims) { c.Sub = "" },
		"empty lease":    func(c *Claims) { c.LeaseID = "" },
		"zero epoch":     func(c *Claims) { c.Epoch = 0 },
		"empty task":     func(c *Claims) { c.TaskID = "" },
		"empty issue":    func(c *Claims) { c.IssueID = "" },
		"empty user":     func(c *Claims) { c.UserID = "" },
		"empty profile":  func(c *Claims) { c.Profile = "" },
		"empty inst":     func(c *Claims) { c.Instance = "" },
		"empty jti":      func(c *Claims) { c.JTI = "" },
		"short jti":      func(c *Claims) { c.JTI = "abc" },
		"zero iat":       func(c *Claims) { c.Iat = 0 },
		"exp not after":  func(c *Claims) { c.Exp = c.Iat },
		"ttl over 15m":   func(c *Claims) { c.Exp = c.Iat + 901 },
	}
	for name, mut := range cases {
		c := claims()
		mut(&c)
		if tok, err := kr.Sign(c); err == nil || tok != "" {
			t.Errorf("%s: signed", name)
		}
	}
}

func TestRotationKeepsOldKeyVerifiable(t *testing.T) {
	old, _ := ParseKeyring("k1:" + seed(1))
	now := time.Unix(tNow, 0)
	oldTok := sign(t, old, claims())

	rot, err := ParseKeyring("k2:" + seed(2) + ",k1:" + seed(1))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := fetchJWKS(t, rot)
	if len(keys) != 2 {
		t.Fatalf("jwks keys=%d", len(keys))
	}
	verify(t, oldTok, keys, now) // still verifies after rotation
	newTok := sign(t, rot, claims())
	_, parsed := verify(t, newTok, keys, now)
	if parsed.Header["kid"] != "k2" {
		t.Fatalf("new tokens must use the first key, got %v", parsed.Header["kid"])
	}
}

func TestJWKSHasNoPrivateMaterial(t *testing.T) {
	kr, _ := ParseKeyring("k1:" + seed(1))
	_, rec := fetchJWKS(t, kr)
	if strings.Contains(rec.Body.String(), `"d"`) {
		t.Fatalf("jwks leaks private member: %s", rec.Body.String())
	}
}

func TestJWKSMethods(t *testing.T) {
	kr, _ := ParseKeyring("k1:" + seed(1))
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		kr.JWKSHandler().ServeHTTP(rec, httptest.NewRequest(m, JWKSPath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s -> %d", m, rec.Code)
		}
	}
}

func TestParseKeyringRejects(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	for name, in := range map[string]string{
		"empty":          "",
		"no colon":       "k1",
		"bad base64":     "k1:!!!",
		"short seed":     "k1:" + short,
		"empty kid":      ":" + seed(1),
		"bad kid chars":  "k 1:" + seed(1),
		"dup kid":        "k1:" + seed(1) + ",k1:" + seed(2),
		"dup key":        "k1:" + seed(1) + ",k2:" + seed(1),
		"trailing comma": "k1:" + seed(1) + ",",
	} {
		if _, err := ParseKeyring(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseKeyringErrorDoesNotLeakSecret(t *testing.T) {
	s := seed(7)
	_, err := ParseKeyring("k 1:" + s)
	if err == nil || strings.Contains(err.Error(), s) {
		t.Fatalf("err=%v", err)
	}
}

func TestKeyringFromEnv(t *testing.T) {
	if _, err := KeyringFromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("missing secret must fail closed")
	}
	kr, err := KeyringFromEnv(func(k string) string {
		if k != SigningKeysEnv {
			t.Fatalf("read %s", k)
		}
		return " k1:" + seed(1) + " "
	})
	if err != nil || kr == nil {
		t.Fatalf("kr=%v err=%v", kr, err)
	}
}
