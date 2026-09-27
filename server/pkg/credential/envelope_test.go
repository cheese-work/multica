package credential

import (
	"bytes"
	"testing"
)

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	kr, err := NewKeyring([]VersionedKey{
		{ID: "k1", Key: bytes.Repeat([]byte{0x01}, KeySize)},
	})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}

func TestSealOpen_RoundTrip(t *testing.T) {
	kr := testKeyring(t)
	aad := AAD{WorkspaceID: "ws-1", RecordID: "rec-1", Purpose: "jev_api_key"}

	env, err := kr.Seal([]byte("super-secret-plaintext"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if env.KeyID != "k1" {
		t.Fatalf("KeyID = %q, want k1", env.KeyID)
	}
	if env.Algorithm != AlgorithmAES256GCM {
		t.Fatalf("Algorithm = %q", env.Algorithm)
	}
	if len(env.Nonce) == 0 || len(env.Ciphertext) == 0 {
		t.Fatalf("expected non-empty nonce and ciphertext")
	}

	plaintext, err := kr.Open(env, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(plaintext) != "super-secret-plaintext" {
		t.Fatalf("plaintext = %q", plaintext)
	}
}

func TestSeal_NonDeterministicNonce(t *testing.T) {
	kr := testKeyring(t)
	aad := AAD{WorkspaceID: "ws-1", RecordID: "rec-1", Purpose: "jev_api_key"}

	env1, err := kr.Seal([]byte("same-plaintext"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	env2, err := kr.Seal([]byte("same-plaintext"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(env1.Nonce, env2.Nonce) {
		t.Fatalf("nonces must differ between calls")
	}
	if bytes.Equal(env1.Ciphertext, env2.Ciphertext) {
		t.Fatalf("ciphertexts must differ between calls")
	}
}

func TestOpen_AADSubstitutionRejected(t *testing.T) {
	kr := testKeyring(t)
	original := AAD{WorkspaceID: "ws-1", RecordID: "rec-1", Purpose: "jev_api_key"}

	env, err := kr.Seal([]byte("secret"), original)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	cases := []struct {
		name string
		aad  AAD
	}{
		{"different workspace", AAD{WorkspaceID: "ws-2", RecordID: "rec-1", Purpose: "jev_api_key"}},
		{"different record", AAD{WorkspaceID: "ws-1", RecordID: "rec-2", Purpose: "jev_api_key"}},
		{"different purpose", AAD{WorkspaceID: "ws-1", RecordID: "rec-1", Purpose: "other_purpose"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := kr.Open(env, tc.aad); err == nil {
				t.Fatalf("expected Open to reject substituted AAD %+v", tc.aad)
			}
		})
	}
}

func TestOpen_TamperedCiphertextRejected(t *testing.T) {
	kr := testKeyring(t)
	aad := AAD{WorkspaceID: "ws-1", RecordID: "rec-1", Purpose: "jev_api_key"}

	env, err := kr.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tampered := env.Ciphertext[0] ^ 0xFF
	env.Ciphertext = append([]byte{tampered}, env.Ciphertext[1:]...)

	if _, err := kr.Open(env, aad); err == nil {
		t.Fatalf("expected Open to reject tampered ciphertext")
	}
}

func TestOpen_UnknownKeyID(t *testing.T) {
	kr := testKeyring(t)
	aad := AAD{WorkspaceID: "ws-1", RecordID: "rec-1", Purpose: "jev_api_key"}
	env, err := kr.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	env.KeyID = "does-not-exist"

	if _, err := kr.Open(env, aad); err == nil {
		t.Fatalf("expected Open to reject unknown key id")
	}
}

func TestKeyring_RotationOldDecryptNewRewrite(t *testing.T) {
	kr1, err := NewKeyring([]VersionedKey{
		{ID: "k1", Key: bytes.Repeat([]byte{0x01}, KeySize)},
	})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	aad := AAD{WorkspaceID: "ws-1", RecordID: "rec-1", Purpose: "jev_api_key"}
	env, err := kr1.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Rotate: new keyring has a new primary key (k2) but still knows k1 for
	// decrypting old envelopes.
	kr2, err := NewKeyring([]VersionedKey{
		{ID: "k2", Key: bytes.Repeat([]byte{0x02}, KeySize)},
		{ID: "k1", Key: bytes.Repeat([]byte{0x01}, KeySize)},
	})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	// Old-key decrypt still works.
	plaintext, err := kr2.Open(env, aad)
	if err != nil {
		t.Fatalf("Open with rotated keyring: %v", err)
	}
	if string(plaintext) != "secret" {
		t.Fatalf("plaintext = %q", plaintext)
	}

	// New-key rewrite: re-sealing produces an envelope keyed by the new
	// primary.
	rewrapped, err := kr2.Seal(plaintext, aad)
	if err != nil {
		t.Fatalf("Seal (rewrap): %v", err)
	}
	if rewrapped.KeyID != "k2" {
		t.Fatalf("rewrapped KeyID = %q, want k2", rewrapped.KeyID)
	}

	// The old keyring (which no longer knows k2) can no longer open the
	// rewrapped envelope — proves rotation actually moved the data forward
	// rather than leaving it decryptable under the retired key.
	if _, err := kr1.Open(rewrapped, aad); err == nil {
		t.Fatalf("expected old keyring to reject envelope sealed under retired-forward key")
	}
}

func TestNewKeyring_RejectsEmpty(t *testing.T) {
	if _, err := NewKeyring(nil); err == nil {
		t.Fatalf("expected error for empty keyring")
	}
}

func TestNewKeyring_RejectsWrongKeySize(t *testing.T) {
	_, err := NewKeyring([]VersionedKey{{ID: "k1", Key: []byte("too-short")}})
	if err == nil {
		t.Fatalf("expected error for wrong key size")
	}
}

func TestNewKeyring_RejectsDuplicateKeyID(t *testing.T) {
	_, err := NewKeyring([]VersionedKey{
		{ID: "k1", Key: bytes.Repeat([]byte{0x01}, KeySize)},
		{ID: "k1", Key: bytes.Repeat([]byte{0x02}, KeySize)},
	})
	if err == nil {
		t.Fatalf("expected error for duplicate key id")
	}
}

func TestNewKeyring_RejectsBlankKeyID(t *testing.T) {
	_, err := NewKeyring([]VersionedKey{{ID: "  ", Key: bytes.Repeat([]byte{0x01}, KeySize)}})
	if err == nil {
		t.Fatalf("expected error for blank key id")
	}
}

func TestPrimaryKeyID(t *testing.T) {
	kr := testKeyring(t)
	if kr.PrimaryKeyID() != "k1" {
		t.Fatalf("PrimaryKeyID() = %q, want k1", kr.PrimaryKeyID())
	}
}
