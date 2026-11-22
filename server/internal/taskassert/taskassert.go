// Package taskassert mints and verifies the signed doop task assertion
// (plan CHE-7064 r3, D1.6).
//
// Token: "mta1." + base64url(JSON claims) + "." + base64url(Ed25519 signature
// over the ASCII bytes "mta1." + payload). base64url is unpadded.
// testdata/vector.json pins the format; doop copies it.
package taskassert

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	prefix   = "mta1."
	audience = "doop"
	ttl      = 24 * time.Hour
	maxSkew  = 60 * time.Second
)

// Claims field order is the wire order; keep it.
type Claims struct {
	V   int    `json:"v"`
	Kid string `json:"kid"`
	Aud string `json:"aud"`
	Wid string `json:"wid"`
	Tid string `json:"tid"`
	Aid string `json:"aid"`
	Iid string `json:"iid"` // empty for a chat task
	Pid string `json:"pid"`
	Prv string `json:"prv"`
	Mdl string `json:"mdl"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

type Key struct {
	Kid  string
	Priv ed25519.PrivateKey
}

var now = time.Now // swapped in tests

var b64 = base64.RawURLEncoding

// ParseKey reads "<kid>:<standard base64 of the 32-byte Ed25519 seed>".
func ParseKey(s string) (Key, error) {
	kid, seedB64, ok := strings.Cut(s, ":")
	if !ok || kid == "" {
		return Key{}, errors.New("want <kid>:<base64 ed25519 seed>")
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return Key{}, errors.New("seed is not base64")
	}
	if len(seed) != ed25519.SeedSize {
		return Key{}, fmt.Errorf("seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return Key{Kid: kid, Priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// Mint signs c. It pins v, kid and aud; a zero iat means now, a zero exp means iat+24h.
func Mint(c Claims, k Key) (string, error) {
	if len(k.Priv) != ed25519.PrivateKeySize || k.Kid == "" {
		return "", errors.New("invalid signing key")
	}
	c.V, c.Kid, c.Aud = 1, k.Kid, audience
	if c.Iat == 0 {
		c.Iat = now().Unix()
	}
	if c.Exp == 0 {
		c.Exp = c.Iat + int64(ttl/time.Second)
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signed := prefix + b64.EncodeToString(p)
	return signed + "." + b64.EncodeToString(ed25519.Sign(k.Priv, []byte(signed))), nil
}

// Verify accepts a token only when its kid is in keys, the signature is valid,
// v is 1, aud is doop, exp is in the future and iat is at most 60 s ahead.
func Verify(token string, keys map[string]ed25519.PublicKey) (Claims, error) {
	var c Claims
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return c, errors.New("bad prefix")
	}
	p, s, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(s, ".") {
		return c, errors.New("bad shape")
	}
	payload, err := b64.DecodeString(p)
	if err != nil {
		return c, errors.New("bad payload encoding")
	}
	sig, err := b64.DecodeString(s)
	if err != nil {
		return c, errors.New("bad signature encoding")
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, errors.New("bad claims")
	}
	pub, ok := keys[c.Kid]
	if !ok || len(pub) != ed25519.PublicKeySize {
		return Claims{}, errors.New("unknown kid")
	}
	if !ed25519.Verify(pub, []byte(prefix+p), sig) {
		return Claims{}, errors.New("bad signature")
	}
	t := now()
	switch {
	case c.V != 1:
		return Claims{}, errors.New("unsupported version")
	case c.Aud != audience:
		return Claims{}, errors.New("wrong audience")
	case c.Exp <= t.Unix():
		return Claims{}, errors.New("expired")
	case c.Iat > t.Add(maxSkew).Unix():
		return Claims{}, errors.New("issued in the future")
	}
	return c, nil
}
