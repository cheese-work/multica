package taskassert

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

type vector struct {
	SigningKey string `json:"signing_key"`
	PublicKey  string `json:"public_key_b64"`
	Claims     Claims `json:"claims"`
	Token      string `json:"token"`
}

func loadVector(t *testing.T) (vector, Key, map[string]ed25519.PublicKey) {
	t.Helper()
	raw, err := os.ReadFile("testdata/vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	key, err := ParseKey(v.SigningKey)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	pub, _ := base64.StdEncoding.DecodeString(v.PublicKey)
	return v, key, map[string]ed25519.PublicKey{key.Kid: pub}
}

func atVector(v vector) { now = func() time.Time { return time.Unix(v.Claims.Iat+60, 0) } }

// The vector is shared with doop S2: Mint must reproduce it byte for byte.
func TestMintMatchesVector(t *testing.T) {
	v, key, _ := loadVector(t)
	got, err := Mint(v.Claims, key)
	if err != nil {
		t.Fatal(err)
	}
	if got != v.Token {
		t.Fatalf("token\n got %s\nwant %s", got, v.Token)
	}
}

func TestVerifyVector(t *testing.T) {
	v, _, keys := loadVector(t)
	atVector(v)
	defer func() { now = time.Now }()
	c, err := Verify(v.Token, keys)
	if err != nil {
		t.Fatal(err)
	}
	if c != v.Claims {
		t.Fatalf("claims %+v want %+v", c, v.Claims)
	}
}

func TestVerifyRejects(t *testing.T) {
	v, key, keys := loadVector(t)
	atVector(v)
	defer func() { now = time.Now }()
	mint := func(mod func(*Claims)) string {
		c := v.Claims
		mod(&c)
		tok, err := Mint(c, key)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	flip := v.Token[:len(v.Token)-3] + "AAA"
	parts := strings.Split(v.Token, ".")
	cases := map[string]string{
		"changed signature byte": flip,
		"changed payload":        parts[0] + "." + parts[1][:len(parts[1])-2] + "AA." + parts[2],
		"wrong prefix":           "mta2." + parts[1] + "." + parts[2],
		"two parts":              parts[0] + "." + parts[1],
		"empty":                  "",
		"expired":                mint(func(c *Claims) { c.Exp = v.Claims.Iat + 30 }),
		"iat too far ahead":      mint(func(c *Claims) { c.Iat = v.Claims.Iat + 200; c.Exp = c.Iat + 100 }),
	}
	for name, tok := range cases {
		if _, err := Verify(tok, keys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Verify(v.Token, map[string]ed25519.PublicKey{"other": keys[key.Kid]}); err == nil {
		t.Error("unknown kid accepted")
	}
	// Mint always pins v and aud, so build bad ones by hand.
	for name, c := range map[string]Claims{"v2": {V: 2, Aud: "doop"}, "aud": {V: 1, Aud: "other"}} {
		c.Kid, c.Iat, c.Exp = key.Kid, v.Claims.Iat, v.Claims.Iat+3600
		p, _ := json.Marshal(c)
		pb := base64.RawURLEncoding.EncodeToString(p)
		sig := ed25519.Sign(key.Priv, []byte("mta1."+pb))
		if _, err := Verify("mta1."+pb+"."+base64.RawURLEncoding.EncodeToString(sig), keys); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestMintDefaultsTimes(t *testing.T) {
	v, key, _ := loadVector(t)
	now = func() time.Time { return time.Unix(1900000000, 0) }
	defer func() { now = time.Now }()
	c := v.Claims
	c.Iat, c.Exp = 0, 0
	tok, err := Mint(c, key)
	if err != nil {
		t.Fatal(err)
	}
	_, keys := func() (int, map[string]ed25519.PublicKey) { _, _, k := loadVector(t); return 0, k }()
	got, err := Verify(tok, keys)
	if err != nil {
		t.Fatal(err)
	}
	if got.Iat != 1900000000 || got.Exp != 1900000000+86400 {
		t.Fatalf("iat/exp = %d/%d", got.Iat, got.Exp)
	}
}

func TestParseKeyMalformed(t *testing.T) {
	for _, s := range []string{"", "k1", ":AAAA", "k1:", "k1:not base64!", "k1:AAEC"} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) accepted", s)
		}
	}
}
