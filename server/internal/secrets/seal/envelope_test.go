package seal

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(id string, fill byte) Key { return Key{ID: id, Material: bytes.Repeat([]byte{fill}, 32)} }

func testBinding() Binding {
	return Binding{WorkspaceID: "workspace-a", CredentialID: "secret-a", Purpose: "secret-purpose"}
}

func TestEnvelopeBindsSecretToWorkspaceAndPurpose(t *testing.T) {
	ring, err := NewKeyring(testKey("current", 1))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := ring.Seal([]byte("fake-secret"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.KeyID != "current" || envelope.Algorithm != AlgorithmAES256GCM || envelope.Version != EnvelopeVersion || envelope.Nonce == "" || envelope.Ciphertext == "" {
		t.Fatalf("incomplete envelope: %#v", envelope)
	}
	if envelope.Ciphertext == "fake-secret" {
		t.Fatal("plaintext stored as ciphertext")
	}
	plaintext, err := ring.Open(envelope, testBinding())
	if err != nil || string(plaintext) != "fake-secret" {
		t.Fatalf("Open = (%q, %v)", plaintext, err)
	}
	for name, binding := range map[string]Binding{
		"workspace": {WorkspaceID: "workspace-b", CredentialID: "secret-a", Purpose: "secret-purpose"},
		"record":    {WorkspaceID: "workspace-a", CredentialID: "other", Purpose: "secret-purpose"},
		"purpose":   {WorkspaceID: "workspace-a", CredentialID: "secret-a", Purpose: "other"},
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
	original, err := old.Seal([]byte("fake-secret"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	rotating, err := NewKeyring(testKey("new", 3), testKey("old", 2))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, replacement, err := rotating.OpenAndRotate(original, testBinding())
	if err != nil || string(plaintext) != "fake-secret" || replacement == nil || replacement.KeyID != "new" {
		t.Fatalf("OpenAndRotate = (%q, %#v, %v)", plaintext, replacement, err)
	}
	if _, err := rotating.Open(*replacement, testBinding()); err != nil {
		t.Fatalf("replacement does not open: %v", err)
	}
	if _, replacement, err := rotating.OpenAndRotate(original, Binding{WorkspaceID: "other", CredentialID: "secret-a", Purpose: "secret-purpose"}); err == nil || replacement != nil {
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

func TestOpenRejectsPurposeMismatch(t *testing.T) {
	ring, err := NewKeyring(testKey("current", 1))
	if err != nil {
		t.Fatal(err)
	}
	env, err := ring.Seal([]byte("fake-secret"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	b.Purpose = "other-purpose"
	if pt, err := ring.Open(env, b); err == nil || pt != nil {
		t.Fatalf("Open with wrong purpose = (%q, %v)", pt, err)
	}
}

func TestOpenAndRotateReturnsNilWhenAlreadyCurrent(t *testing.T) {
	ring, err := NewKeyring(testKey("current", 1))
	if err != nil {
		t.Fatal(err)
	}
	env, err := ring.Seal([]byte("fake-secret"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	if _, repl, err := ring.OpenAndRotate(env, testBinding()); err != nil || repl != nil {
		t.Fatalf("OpenAndRotate current-key = (%#v, %v)", repl, err)
	}
}

func TestErrorsNeverContainKeyMaterial(t *testing.T) {
	material := bytes.Repeat([]byte{9}, 32)
	enc := base64.StdEncoding.EncodeToString(material)
	leaks := []string{enc, string(material), base64.RawStdEncoding.EncodeToString(material)}
	check := func(err error) {
		t.Helper()
		if err == nil {
			return
		}
		for _, l := range leaks {
			if strings.Contains(err.Error(), l) {
				t.Fatalf("error leaks key material: %v", err)
			}
		}
	}
	_, err := DecodeKey("k", enc[:len(enc)-4])
	check(err)
	_, err = NewKeyring(Key{ID: "k", Material: material[:5]})
	check(err)
	_, err = NewKeyring(Key{ID: "k", Material: material}, Key{ID: "k", Material: material})
	check(err)
	ring, err := NewKeyring(Key{ID: "k", Material: material})
	if err != nil {
		t.Fatal(err)
	}
	env, _ := ring.Seal([]byte("fake-secret"), testBinding())
	b := testBinding()
	b.Purpose = "x"
	_, err = ring.Open(env, b)
	if err == nil {
		t.Fatal("expected failure")
	}
	check(err)
	_, err = ring.Open(Envelope{Version: EnvelopeVersion, KeyID: KeyID(material), Algorithm: AlgorithmAES256GCM, Nonce: "bad", Ciphertext: "bad"}, testBinding())
	check(err)
}
