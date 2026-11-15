// Package browserlease signs Steel lease tokens and publishes their verification keys
// (CHE-4733, W3-4; plan W1 r3, "Token model").
//
// Mint (W3-3) calls Keyring.Mint; the internal listener (W3-17) mounts Keyring.JWKSHandler at
// JWKSPath. The public and tailnet routers must not mount it. The private seeds come from the
// C00 secret named by SigningKeysEnv, applied in W2-13 after Cheese's approval.
package browserlease

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	Issuer   = "multica"
	Audience = "steel-gateway"
	TokenTTL = 15 * time.Minute
	// SigningKeysEnv names the C00 secret: comma-separated `kid:base64(32-byte seed)`.
	// The first entry signs; the rest stay in the JWKS so tokens signed before a rotation verify
	// until they expire (drop an old entry after TokenTTL).
	SigningKeysEnv = "MULTICA_BROWSER_LEASE_SIGNING_KEYS"
	JWKSPath       = "/internal/browser/jwks"
)

var kidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Claims are the token claims beyond iss, aud, iat, exp and jti, which Mint sets. Mint writes
// each claim by name into a map; the struct tags document the wire names.
// DecisionID is null only for class S0. Class S1 is deferred and never signed.
type Claims struct {
	Sub        string  `json:"sub"`
	LeaseID    string  `json:"lease_id"`
	Epoch      int64   `json:"epoch"`
	TaskID     string  `json:"task_id"`
	IssueID    string  `json:"issue_id"`
	DecisionID *string `json:"decision_id"`
	UserID     string  `json:"user_id"`
	Profile    string  `json:"profile"`
	Class      string  `json:"class"`
	Instance   string  `json:"instance"`
}

func (c Claims) validate() error {
	switch {
	case c.Sub == "", c.LeaseID == "", c.TaskID == "", c.IssueID == "", c.UserID == "",
		c.Profile == "", c.Instance == "":
		return errors.New("browserlease: empty required claim")
	case c.Epoch <= 0:
		return errors.New("browserlease: epoch must be positive")
	}
	switch c.Class {
	case "S0":
		if c.DecisionID != nil && *c.DecisionID == "" {
			return errors.New("browserlease: empty decision_id")
		}
	case "S2", "H":
		if c.DecisionID == nil || *c.DecisionID == "" {
			return fmt.Errorf("browserlease: class %s needs decision_id", c.Class)
		}
	default:
		return fmt.Errorf("browserlease: class %q is not mintable", c.Class)
	}
	return nil
}

type signingKey struct {
	kid  string
	priv ed25519.PrivateKey
}

// Keyring holds the signing keys. It is immutable after construction.
type Keyring struct {
	keys []signingKey // keys[0] signs
	jwks []byte
}

// KeyringFromEnv reads SigningKeysEnv through getenv. A missing secret is an error: the caller
// must not serve lease tokens unsigned.
func KeyringFromEnv(getenv func(string) string) (*Keyring, error) {
	return ParseKeyring(strings.TrimSpace(getenv(SigningKeysEnv)))
}

// ParseKeyring parses `kid:base64seed[,kid:base64seed...]`. Errors never echo the input.
func ParseKeyring(spec string) (*Keyring, error) {
	if spec == "" {
		return nil, errors.New("browserlease: no signing keys")
	}
	kr := &Keyring{}
	kids := map[string]bool{}
	pubs := map[string]bool{}
	for i, entry := range strings.Split(spec, ",") {
		kid, b64, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || !kidPattern.MatchString(kid) {
			return nil, fmt.Errorf("browserlease: signing key %d: want kid:base64seed with kid [A-Za-z0-9_-]{1,64}", i+1)
		}
		seed, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("browserlease: signing key %q: seed must be %d bytes of standard base64", kid, ed25519.SeedSize)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pub := string(priv.Public().(ed25519.PublicKey))
		if kids[kid] || pubs[pub] {
			return nil, fmt.Errorf("browserlease: signing key %q duplicates another entry", kid)
		}
		kids[kid], pubs[pub] = true, true
		kr.keys = append(kr.keys, signingKey{kid: kid, priv: priv})
	}
	kr.jwks = kr.buildJWKS()
	return kr, nil
}

type jwkEntry struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

// buildJWKS renders the public keys as an RFC 8037 OKP set. Only the public half is encoded.
func (kr *Keyring) buildJWKS() []byte {
	set := struct {
		Keys []jwkEntry `json:"keys"`
	}{Keys: make([]jwkEntry, 0, len(kr.keys))}
	for _, k := range kr.keys {
		pub := k.priv.Public().(ed25519.PublicKey)
		set.Keys = append(set.Keys, jwkEntry{
			Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub),
			Kid: k.kid, Use: "sig", Alg: "EdDSA",
		})
	}
	b, _ := json.Marshal(set) // fixed shape of strings; cannot fail
	return b
}

// JWKSHandler serves the public keys. GET and HEAD only.
func (kr *Keyring) JWKSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(kr.jwks)
	})
}

// Mint signs a token for c with the first key: iat=now, exp=now+TokenTTL, a random jti.
// The caller stores a hash of jti, never the token. The caller enforces the hard lease limit
// (2 h or the decision's duration) before calling Mint on a renew.
func (kr *Keyring) Mint(c Claims, now time.Time) (token, jti string, exp time.Time, err error) {
	if err := c.validate(); err != nil {
		return "", "", time.Time{}, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", time.Time{}, fmt.Errorf("browserlease: jti: %w", err)
	}
	jti = hex.EncodeToString(raw[:])
	exp = now.Add(TokenTTL)
	mc := jwt.MapClaims{
		"iss": Issuer, "aud": Audience, "jti": jti, "sub": c.Sub, "lease_id": c.LeaseID,
		"epoch": c.Epoch, "task_id": c.TaskID, "issue_id": c.IssueID, "decision_id": c.DecisionID,
		"user_id": c.UserID, "profile": c.Profile, "class": c.Class, "instance": c.Instance,
		"iat": now.Unix(), "exp": exp.Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, mc)
	t.Header["kid"] = kr.keys[0].kid
	token, err = t.SignedString(kr.keys[0].priv)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("browserlease: sign: %w", err)
	}
	return token, jti, exp, nil
}
