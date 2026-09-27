package credential

import (
	"encoding/base64"
	"testing"
)

func b64Key(b byte) string {
	return base64.StdEncoding.EncodeToString(make32(b))
}

func make32(b byte) []byte {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = b
	}
	return key
}

func TestLoadKeyringFromEnv_Absent(t *testing.T) {
	t.Setenv("CHE714_TEST_KEYS", "")
	kr, err := LoadKeyringFromEnv("CHE714_TEST_KEYS")
	if err != nil {
		t.Fatalf("LoadKeyringFromEnv: %v", err)
	}
	if kr != nil {
		t.Fatalf("expected nil keyring when env var is unset, got %+v", kr)
	}
}

func TestLoadKeyringFromEnv_SingleKey(t *testing.T) {
	t.Setenv("CHE714_TEST_KEYS", "k1:"+b64Key(0x01))
	kr, err := LoadKeyringFromEnv("CHE714_TEST_KEYS")
	if err != nil {
		t.Fatalf("LoadKeyringFromEnv: %v", err)
	}
	if kr == nil {
		t.Fatalf("expected non-nil keyring")
	}
	if kr.PrimaryKeyID() != "k1" {
		t.Fatalf("PrimaryKeyID() = %q, want k1", kr.PrimaryKeyID())
	}
}

func TestLoadKeyringFromEnv_RotationOrderPreserved(t *testing.T) {
	// New primary listed first, retired key second — LoadKeyringFromEnv must
	// preserve that order so Seal uses k2, not k1.
	t.Setenv("CHE714_TEST_KEYS", "k2:"+b64Key(0x02)+",k1:"+b64Key(0x01))
	kr, err := LoadKeyringFromEnv("CHE714_TEST_KEYS")
	if err != nil {
		t.Fatalf("LoadKeyringFromEnv: %v", err)
	}
	if kr.PrimaryKeyID() != "k2" {
		t.Fatalf("PrimaryKeyID() = %q, want k2", kr.PrimaryKeyID())
	}

	aad := AAD{WorkspaceID: "ws", RecordID: "rec", Purpose: "p"}
	env, err := kr.Seal([]byte("x"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if env.KeyID != "k2" {
		t.Fatalf("Seal used key %q, want k2", env.KeyID)
	}
}

func TestLoadKeyringFromEnv_MalformedEntry(t *testing.T) {
	cases := []string{
		"not-a-valid-entry",
		"k1:not-base64!!!",
		"k1:" + b64Key(0x01) + ",k1:" + b64Key(0x02), // duplicate id
		":" + b64Key(0x01),                           // blank id
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("CHE714_TEST_KEYS", raw)
			if _, err := LoadKeyringFromEnv("CHE714_TEST_KEYS"); err == nil {
				t.Fatalf("expected error for malformed entry %q", raw)
			}
		})
	}
}
