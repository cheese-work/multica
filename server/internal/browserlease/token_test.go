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

func claims() Claims {
	return Claims{
		Sub: "agent-1", LeaseID: "lease-1", Epoch: 3, TaskID: "task-1", IssueID: "issue-1",
		DecisionID: str("dec-1"), UserID: "user-1", Profile: "figma", Class: "S2", Instance: "c00-1", Host: "c00",
	}
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

func TestMintClaimsAndJWKS(t *testing.T) {
	kr, err := ParseKeyring("k1:" + seed(1))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	tok, jti, exp, err := kr.Mint(claims(), now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(15 * time.Minute); !exp.Equal(want) {
		t.Fatalf("exp=%v want %v", exp, want)
	}
	keys, rec := fetchJWKS(t, kr)
	mc, parsed := verify(t, tok, keys, now.Add(time.Minute))
	if parsed.Header["kid"] != "k1" || parsed.Header["alg"] != "EdDSA" {
		t.Fatalf("header %v", parsed.Header)
	}
	want := map[string]any{
		"iss": Issuer, "aud": "steel-gateway/c00", "sub": "agent-1", "lease_id": "lease-1", "epoch": float64(3),
		"task_id": "task-1", "issue_id": "issue-1", "decision_id": "dec-1", "user_id": "user-1",
		"profile": "figma", "class": "S2", "instance": "c00-1", "jti": jti,
		"iat": float64(now.Unix()), "exp": float64(exp.Unix()),
	}
	if len(mc) != len(want) {
		t.Fatalf("claims %v", mc)
	}
	for k, v := range want {
		if mc[k] != v {
			t.Errorf("claim %s = %v, want %v", k, mc[k], v)
		}
	}
	if len(jti) < 32 {
		t.Fatalf("jti too short: %q", jti)
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
	now := time.Now()
	for _, tc := range []struct{ host, class, inst, aud, other string }{
		{"c00", "S2", "c00-1", "steel-gateway/c00", "steel-gateway/x99"},
		{"x99", "S0", "x99-3", "steel-gateway/x99", "steel-gateway/c00"},
	} {
		c := claims()
		c.Host, c.Class, c.Instance = tc.host, tc.class, tc.inst
		if tc.class == "S0" {
			c.DecisionID = nil
		}
		tok, _, _, err := kr.Mint(c, now)
		if err != nil {
			t.Fatalf("%s: %v", tc.host, err)
		}
		mc, _ := verifyAud(t, tok, keys, now, tc.aud)
		if mc["aud"] != tc.aud {
			t.Errorf("%s: aud=%v want %s", tc.host, mc["aud"], tc.aud)
		}
		if _, err := jwt.NewParser(jwt.WithAudience(tc.other), jwt.WithTimeFunc(func() time.Time { return now })).
			Parse(tok, func(*jwt.Token) (any, error) { return keys["k1"], nil }); err == nil {
			t.Errorf("%s token accepted by %s verifier", tc.host, tc.other)
		}
		if _, err := jwt.NewParser(jwt.WithAudience("steel-gateway"), jwt.WithTimeFunc(func() time.Time { return now })).
			Parse(tok, func(*jwt.Token) (any, error) { return keys["k1"], nil }); err == nil {
			t.Errorf("%s token accepted with generic audience", tc.host)
		}
	}
}

func TestJTIUnique(t *testing.T) {
	kr, _ := ParseKeyring("k1:" + seed(1))
	_, a, _, _ := kr.Mint(claims(), time.Now())
	_, b, _, _ := kr.Mint(claims(), time.Now())
	if a == "" || a == b {
		t.Fatalf("jti %q %q", a, b)
	}
}

func TestDecisionNullOnlyForS0(t *testing.T) {
	kr, _ := ParseKeyring("k1:" + seed(1))
	keys, _ := fetchJWKS(t, kr)
	now := time.Now()
	c := claims()
	c.Class, c.DecisionID, c.Instance, c.Host = "S0", nil, "x99-1", "x99"
	tok, _, _, err := kr.Mint(c, now)
	if err != nil {
		t.Fatalf("S0 null decision: %v", err)
	}
	mc, _ := verifyAud(t, tok, keys, now, "steel-gateway/x99")
	if v, ok := mc["decision_id"]; !ok || v != nil {
		t.Fatalf("decision_id must be present and null, got %v (present=%v)", v, ok)
	}
	for _, class := range []string{"S2", "H"} {
		c.Class = class
		if _, _, _, err := kr.Mint(c, now); err == nil {
			t.Errorf("class %s minted without decision", class)
		}
	}
}

func TestMintRejectsBadClaims(t *testing.T) {
	kr, _ := ParseKeyring("k1:" + seed(1))
	cases := map[string]func(*Claims){
		"S1 class":       func(c *Claims) { c.Class = "S1" },
		"unknown class":  func(c *Claims) { c.Class = "x" },
		"empty sub":      func(c *Claims) { c.Sub = "" },
		"empty lease":    func(c *Claims) { c.LeaseID = "" },
		"zero epoch":     func(c *Claims) { c.Epoch = 0 },
		"empty task":     func(c *Claims) { c.TaskID = "" },
		"empty issue":    func(c *Claims) { c.IssueID = "" },
		"empty user":     func(c *Claims) { c.UserID = "" },
		"empty profile":  func(c *Claims) { c.Profile = "" },
		"empty inst":     func(c *Claims) { c.Instance = "" },
		"empty host":     func(c *Claims) { c.Host = "" },
		"unknown host":   func(c *Claims) { c.Host = "mbp" },
		"generic host":   func(c *Claims) { c.Host = "steel-gateway" },
		"empty decision": func(c *Claims) { c.DecisionID = str("") },
	}
	for name, mut := range cases {
		c := claims()
		mut(&c)
		if _, _, _, err := kr.Mint(c, time.Now()); err == nil {
			t.Errorf("%s: minted", name)
		}
	}
}

func TestRotationKeepsOldKeyVerifiable(t *testing.T) {
	old, _ := ParseKeyring("k1:" + seed(1))
	now := time.Now()
	oldTok, _, _, _ := old.Mint(claims(), now)

	rot, err := ParseKeyring("k2:" + seed(2) + ",k1:" + seed(1))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := fetchJWKS(t, rot)
	if len(keys) != 2 {
		t.Fatalf("jwks keys=%d", len(keys))
	}
	verify(t, oldTok, keys, now) // still verifies after rotation
	newTok, _, _, _ := rot.Mint(claims(), now)
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
