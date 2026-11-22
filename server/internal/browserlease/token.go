// Package browserlease signs Steel lease tokens and publishes their verification keys
// (CHE-4733, W3-4; plan W1 r5, "Token model").
//
// W3-3's Service signs through Keyring.Sign (the Signer interface); the internal listener (W3-17) mounts Keyring.JWKSHandler at
// JWKSPath. The public and tailnet routers must not mount it. The private seeds come from the
// C00 secret named by SigningKeysEnv, applied in W2-13 after Cheese's approval.
package browserlease

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

const (
	Issuer = "multica"
	// SigningKeysEnv names the C00 secret: comma-separated `kid:base64(32-byte seed)`.
	// The first entry signs; the rest stay in the JWKS so tokens signed before a rotation verify
	// until they expire (drop an old entry after TokenTTL).
	SigningKeysEnv = "MULTICA_BROWSER_LEASE_SIGNING_KEYS"
	JWKSPath       = "/internal/browser/jwks"
)

var kidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type signingKey struct {
	kid  string
	priv ed25519.PrivateKey
}

// Keyring holds the signing keys. It is immutable after construction.
type Keyring struct {
	keys  []signingKey // keys[0] signs
	jwks  []byte
	allow AudienceSet // audiences this keyring may sign; empty signs nothing
}

// WithAudiences returns a copy of kr that signs only the given audiences.
func (kr *Keyring) WithAudiences(allow AudienceSet) *Keyring {
	c := *kr
	c.allow = allow
	return &c
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
		pub := string(priv[ed25519.SeedSize:])
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
		pub := k.priv[ed25519.SeedSize:] // public half of the expanded key
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

// Sign implements Signer: it signs the claims W3-3's Service built with the first key and sets
// `iss` and the `kid` header. It writes each claim by name, so `decision_id` is present and null
// for class S0. The token carries the caller's `aud`, which must name a host.
func (kr *Keyring) Sign(c Claims) (string, error) {
	if err := validateClaims(c, kr.allow); err != nil {
		return "", err
	}
	var decision, task, issue *string
	if c.DecisionID != "" {
		decision = &c.DecisionID
	}
	if c.TaskID != "" {
		task = &c.TaskID
	}
	if c.IssueID != "" {
		issue = &c.IssueID
	}
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": Issuer, "aud": c.Aud, "jti": c.JTI, "sub": c.Sub, "lease_id": c.LeaseID,
		"epoch": c.Epoch, "task_id": task, "issue_id": issue, "decision_id": decision,
		"user_id": c.UserID, "profile": c.Profile, "class": c.Class, "instance": c.Instance,
		"iat": c.Iat, "exp": c.Exp,
	})
	t.Header["kid"] = kr.keys[0].kid
	tok, err := t.SignedString(kr.keys[0].priv)
	if err != nil {
		return "", fmt.Errorf("browserlease: sign: %w", err)
	}
	return tok, nil
}

var _ Signer = (*Keyring)(nil)

// validateClaims refuses what no gateway would accept: an `aud` other than the two host
// audiences (each gateway accepts only its own), an empty required claim, a lifetime over
// TokenTTL, and a class that is not mintable. Class S1 is deferred and never signed.
func validateClaims(c Claims, allow AudienceSet) error {
	if c.Aud != Aud(HostC00) && c.Aud != Aud(HostX99) {
		return fmt.Errorf("browserlease: aud %q is not %s or %s", c.Aud, Aud(HostC00), Aud(HostX99))
	}
	switch {
	case c.Sub == "", c.LeaseID == "", c.UserID == "", c.Profile == "", c.Instance == "":
		return errors.New("browserlease: empty required claim")
	case c.Class != ClassH && (c.TaskID == "" || c.IssueID == ""):
		// Only a member's login lease has no task or issue.
		return errors.New("browserlease: empty required claim")
	case len(c.JTI) < 32:
		return errors.New("browserlease: jti must be at least 32 characters")
	case c.Epoch <= 0:
		return errors.New("browserlease: epoch must be positive")
	case c.Iat <= 0 || c.Exp <= c.Iat || c.Exp-c.Iat > int64(TokenTTL.Seconds()):
		return errors.New("browserlease: exp must be after iat and within the token TTL")
	}
	switch c.Class {
	case ClassS0:
	case ClassS2, ClassH:
		if c.DecisionID == "" {
			return fmt.Errorf("browserlease: class %s needs decision_id", c.Class)
		}
	default:
		return fmt.Errorf("browserlease: class %q is not mintable", c.Class)
	}
	return nil
}
