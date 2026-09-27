package credential

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func testKey(id string, fill byte) Key { return Key{ID: id, Material: bytes.Repeat([]byte{fill}, 32)} }

func testBinding() Binding {
	return Binding{WorkspaceID: "workspace-a", CredentialID: "jev-api-key", Purpose: "jev-provider-credential"}
}

func TestEnvelopeBindsCredentialToWorkspaceAndPurpose(t *testing.T) {
	ring, err := NewKeyring(testKey("current", 1))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := ring.Seal([]byte("fake-jev-secret"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.KeyID != "current" || envelope.Algorithm != AlgorithmAES256GCM || envelope.Version != EnvelopeVersion || envelope.Nonce == "" || envelope.Ciphertext == "" {
		t.Fatalf("incomplete envelope: %#v", envelope)
	}
	if envelope.Ciphertext == "fake-jev-secret" {
		t.Fatal("plaintext stored as ciphertext")
	}
	plaintext, err := ring.Open(envelope, testBinding())
	if err != nil || string(plaintext) != "fake-jev-secret" {
		t.Fatalf("Open = (%q, %v)", plaintext, err)
	}
	for name, binding := range map[string]Binding{
		"workspace": {WorkspaceID: "workspace-b", CredentialID: "jev-api-key", Purpose: "jev-provider-credential"},
		"record":    {WorkspaceID: "workspace-a", CredentialID: "other", Purpose: "jev-provider-credential"},
		"purpose":   {WorkspaceID: "workspace-a", CredentialID: "jev-api-key", Purpose: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ring.Open(envelope, binding); err == nil {
				t.Fatal("AAD substitution decrypted the credential")
			}
		})
	}
}

func TestEnvelopeRotationRewritesOnlyAfterOldKeyDecrypts(t *testing.T) {
	old, err := NewKeyring(testKey("old", 2))
	if err != nil {
		t.Fatal(err)
	}
	original, err := old.Seal([]byte("fake-jev-secret"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	rotating, err := NewKeyring(testKey("new", 3), testKey("old", 2))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, replacement, err := rotating.OpenAndRotate(original, testBinding())
	if err != nil || string(plaintext) != "fake-jev-secret" || replacement == nil || replacement.KeyID != "new" {
		t.Fatalf("OpenAndRotate = (%q, %#v, %v)", plaintext, replacement, err)
	}
	if _, err := rotating.Open(*replacement, testBinding()); err != nil {
		t.Fatalf("replacement does not open: %v", err)
	}
	if _, replacement, err := rotating.OpenAndRotate(original, Binding{WorkspaceID: "other", CredentialID: "jev-api-key", Purpose: "jev-provider-credential"}); err == nil || replacement != nil {
		t.Fatalf("failed rotation changed stored state: replacement=%#v err=%v", replacement, err)
	}
	if original.KeyID != "old" {
		t.Fatal("failed rotation mutated original envelope")
	}
}

func TestEnvelopeRejectsMalformedAndUnknownKey(t *testing.T) {
	ring, err := NewKeyring(testKey("current", 1))
	if err != nil {
		t.Fatal(err)
	}
	for name, envelope := range map[string]Envelope{
		"unknown-key": {Version: EnvelopeVersion, KeyID: "gone", Algorithm: AlgorithmAES256GCM, Nonce: "AA==", Ciphertext: "AA=="},
		"wrong-algo":  {Version: EnvelopeVersion, KeyID: "current", Algorithm: "AES-128-GCM", Nonce: "AA==", Ciphertext: "AA=="},
		"bad-nonce":   {Version: EnvelopeVersion, KeyID: "current", Algorithm: AlgorithmAES256GCM, Nonce: "not-base64", Ciphertext: "AA=="},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ring.Open(envelope, testBinding()); err == nil {
				t.Fatal("malformed envelope opened")
			}
		})
	}
}

func TestDecodeKeyAndKeyIDRejectInvalidDeploymentMaterial(t *testing.T) {
	material := bytes.Repeat([]byte{7}, 32)
	key, err := DecodeKey("current", base64.StdEncoding.EncodeToString(material))
	if err != nil || !bytes.Equal(key.Material, material) || KeyID(material) == KeyID(bytes.Repeat([]byte{8}, 32)) {
		t.Fatalf("valid deployment key = (%#v, %v)", key, err)
	}
	for _, encoded := range []string{"", "not-base64", base64.StdEncoding.EncodeToString(material[:31])} {
		if _, err := DecodeKey("current", encoded); err == nil {
			t.Fatalf("invalid deployment key accepted: %q", encoded)
		}
	}
}

func TestNewKeyringRejectsDistinctMaterialWithTheSameID(t *testing.T) {
	_, err := NewKeyring(
		Key{ID: "collision", Material: bytes.Repeat([]byte{1}, 32)},
		Key{ID: "collision", Material: bytes.Repeat([]byte{2}, 32)},
	)
	if err == nil {
		t.Fatal("keyring accepted distinct key material with the same ID")
	}
}
