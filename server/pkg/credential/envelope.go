// Package credential provides versioned, AAD-bound authenticated encryption
// for write-only credentials stored at rest (CHE-714) — starting with the
// Jev/TypeSafe API key, but shaped for any per-workspace secret that must
// support key rotation without a flag day.
//
// Construction: AES-256-GCM. Every envelope records the key id, algorithm,
// nonce, and ciphertext, plus the associated data (AAD) it was sealed under.
// The AAD binds a ciphertext to the workspace, credential record, and
// purpose it belongs to, so a row copied to a different workspace or record
// fails to decrypt instead of silently opening.
//
// Rotation: a [Keyring] holds a primary key (used for all new Seal calls)
// and any number of retired keys (tried in order for Open). Rotating a
// credential is: Open under the old keyring, Seal the recovered plaintext
// under the new keyring, persist the new envelope. Nothing decrypts the
// value in place — the plaintext exists only transiently, in memory, during
// that rewrap.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the required AES-256 key length in bytes.
const KeySize = 32

// AlgorithmAES256GCM identifies the only algorithm this package currently
// seals with. Envelopes record it explicitly rather than assuming it, so a
// future algorithm change is detectable on read instead of silently
// misinterpreted.
const AlgorithmAES256GCM = "AES-256-GCM"

var (
	// ErrNoKeys is returned by NewKeyring when given no keys.
	ErrNoKeys = errors.New("credential: keyring requires at least one key")
	// ErrInvalidKeySize is returned when a key is not KeySize bytes.
	ErrInvalidKeySize = fmt.Errorf("credential: key must be %d bytes", KeySize)
	// ErrBlankKeyID is returned when a key id is empty or whitespace-only.
	ErrBlankKeyID = errors.New("credential: key id must not be blank")
	// ErrDuplicateKeyID is returned when two keys share an id.
	ErrDuplicateKeyID = errors.New("credential: duplicate key id")
	// ErrUnknownKeyID is returned by Open when no keyring key matches the
	// envelope's KeyID — the key was retired past keeping, or the envelope
	// belongs to a different keyring entirely.
	ErrUnknownKeyID = errors.New("credential: unknown key id")
	// ErrAuthenticationFailed is returned by Open when GCM authentication
	// fails — a tampered ciphertext, a wrong key, or (this package's main
	// use of it) AAD that does not match what the value was sealed under.
	ErrAuthenticationFailed = errors.New("credential: authentication failed")
)

// AAD is the associated data an envelope is bound to. It is authenticated
// but not encrypted — it never appears inside the ciphertext — and any
// mismatch on Open fails closed rather than returning garbage plaintext.
//
// Binding all three fields means a credential row cannot be replayed into a
// different workspace, swapped with a sibling record's ciphertext, or
// reinterpreted under a different purpose, even by someone with direct
// database access and the right key.
type AAD struct {
	WorkspaceID string
	RecordID    string
	Purpose     string
}

// bytes renders the AAD deterministically for GCM. Length-prefixing each
// field prevents ambiguity between e.g. WorkspaceID="ab"+RecordID="c" and
// WorkspaceID="a"+RecordID="bc", which plain concatenation would conflate.
func (a AAD) bytes() []byte {
	var b strings.Builder
	for _, field := range []string{a.WorkspaceID, a.RecordID, a.Purpose} {
		fmt.Fprintf(&b, "%d:%s|", len(field), field)
	}
	return []byte(b.String())
}

// Envelope is the persisted, versioned encryption of one secret value.
type Envelope struct {
	KeyID      string
	Algorithm  string
	Nonce      []byte
	Ciphertext []byte
}

// VersionedKey is one keyring entry.
type VersionedKey struct {
	ID  string
	Key []byte
}

// Keyring seals with its primary key and opens under any key it holds,
// letting Open succeed for envelopes sealed under a since-retired key while
// every new Seal call moves data forward onto the current one.
type Keyring struct {
	primaryID string
	aeads     map[string]cipher.AEAD
}

// NewKeyring builds a Keyring from an ordered list of keys. The first entry
// is the primary — the one Seal uses — and every entry (including the
// primary) is available to Open. Order after the first does not matter for
// Open, but callers conventionally list keys newest-first.
func NewKeyring(keys []VersionedKey) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}
	aeads := make(map[string]cipher.AEAD, len(keys))
	for _, k := range keys {
		id := strings.TrimSpace(k.ID)
		if id == "" {
			return nil, ErrBlankKeyID
		}
		if _, exists := aeads[id]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateKeyID, id)
		}
		if len(k.Key) != KeySize {
			return nil, fmt.Errorf("%w: key %q is %d bytes", ErrInvalidKeySize, id, len(k.Key))
		}
		block, err := aes.NewCipher(k.Key)
		if err != nil {
			return nil, fmt.Errorf("credential: aes.NewCipher for key %q: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("credential: cipher.NewGCM for key %q: %w", id, err)
		}
		aeads[id] = aead
	}
	return &Keyring{primaryID: strings.TrimSpace(keys[0].ID), aeads: aeads}, nil
}

// PrimaryKeyID returns the key id new Seal calls use.
func (kr *Keyring) PrimaryKeyID() string { return kr.primaryID }

// Seal encrypts plaintext under the primary key, binding aad as associated
// data. The nonce is freshly random per call: two Seal calls on identical
// plaintext and AAD produce different ciphertexts, so a ciphertext must
// never be used as a lookup key for its plaintext.
func (kr *Keyring) Seal(plaintext []byte, aad AAD) (Envelope, error) {
	primary, ok := kr.aeads[kr.primaryID]
	if !ok {
		return Envelope{}, fmt.Errorf("%w: primary %q", ErrUnknownKeyID, kr.primaryID)
	}
	nonce := make([]byte, primary.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, fmt.Errorf("credential: read nonce: %w", err)
	}
	ciphertext := primary.Seal(nil, nonce, plaintext, aad.bytes())
	return Envelope{
		KeyID:      kr.primaryID,
		Algorithm:  AlgorithmAES256GCM,
		Nonce:      nonce,
		Ciphertext: ciphertext,
	}, nil
}

// Open decrypts env, verifying it was sealed under exactly aad. Wrong AAD
// (workspace, record, or purpose substituted), a tampered ciphertext, or a
// key id the keyring does not hold all fail the same way: an error, no
// plaintext, no partial result.
func (kr *Keyring) Open(env Envelope, aad AAD) ([]byte, error) {
	if env.Algorithm != AlgorithmAES256GCM {
		return nil, fmt.Errorf("credential: unsupported algorithm %q", env.Algorithm)
	}
	aead, ok := kr.aeads[env.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKeyID, env.KeyID)
	}
	plaintext, err := aead.Open(nil, env.Nonce, env.Ciphertext, aad.bytes())
	if err != nil {
		return nil, ErrAuthenticationFailed
	}
	return plaintext, nil
}

// NeedsRotation reports whether env was sealed under a key other than the
// keyring's current primary — the signal a rotation sweep uses to decide
// which rows still need Open-then-reseal.
func (kr *Keyring) NeedsRotation(env Envelope) bool {
	return env.KeyID != kr.primaryID
}
