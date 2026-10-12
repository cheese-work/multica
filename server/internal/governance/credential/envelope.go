// Package credential stores Jev credentials in versioned AEAD envelopes.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	EnvelopeVersion    = 1
	AlgorithmAES256GCM = "AES-256-GCM"
)

var (
	ErrInvalidKey     = errors.New("credential: key must be 32 bytes")
	ErrInvalidBinding = errors.New("credential: workspace, record, and purpose are required")
	ErrUnsupported    = errors.New("credential: unsupported envelope")
	ErrUnknownKey     = errors.New("credential: key is unavailable")
	ErrMalformed      = errors.New("credential: malformed envelope")
)

type Key struct {
	ID       string
	Material []byte
}
type Binding struct {
	WorkspaceID  string
	CredentialID string
	Purpose      string
}
type Envelope struct {
	Version    int    `json:"version"`
	KeyID      string `json:"key_id"`
	Algorithm  string `json:"algorithm"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}
type keyringEntry struct{ aead cipher.AEAD }
type Keyring struct {
	current string
	keys    map[string]keyringEntry
}

func NewKeyring(keys ...Key) (*Keyring, error) {
	if len(keys) == 0 || keys[0].ID == "" {
		return nil, ErrInvalidKey
	}
	ring := &Keyring{current: keys[0].ID, keys: make(map[string]keyringEntry, len(keys))}
	for _, key := range keys {
		if key.ID == "" || len(key.Material) != 32 {
			return nil, ErrInvalidKey
		}
		if _, found := ring.keys[key.ID]; found {
			return nil, fmt.Errorf("credential: duplicate key id %q", key.ID)
		}
		block, err := aes.NewCipher(key.Material)
		if err != nil {
			return nil, fmt.Errorf("credential: aes cipher: %w", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("credential: gcm: %w", err)
		}
		ring.keys[key.ID] = keyringEntry{aead: aead}
	}
	return ring, nil
}

// DecodeKey decodes the base64 deployment-key format used by Multica's other
// at-rest secret stores. It accepts no plaintext key material.
func DecodeKey(id, encoded string) (Key, error) {
	material, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || id == "" || len(material) != 32 {
		return Key{}, ErrInvalidKey
	}
	return Key{ID: id, Material: material}, nil
}

// KeyID returns a stable, non-secret identifier for a deployment key. It lets
// a keyring distinguish current from prior key material during rotation.
func KeyID(material []byte) string {
	digest := sha256.Sum256(material)
	return fmt.Sprintf("k-%x", digest[:8])
}

func (r *Keyring) Seal(plaintext []byte, binding Binding) (Envelope, error) {
	if err := binding.validate(); err != nil {
		return Envelope{}, err
	}
	entry := r.keys[r.current]
	nonce := make([]byte, entry.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, fmt.Errorf("credential: nonce: %w", err)
	}
	return Envelope{Version: EnvelopeVersion, KeyID: r.current, Algorithm: AlgorithmAES256GCM,
		Nonce: base64.RawStdEncoding.EncodeToString(nonce), Ciphertext: base64.RawStdEncoding.EncodeToString(entry.aead.Seal(nil, nonce, plaintext, binding.aad()))}, nil
}

func (r *Keyring) Open(envelope Envelope, binding Binding) ([]byte, error) {
	if err := binding.validate(); err != nil {
		return nil, err
	}
	if envelope.Version != EnvelopeVersion || envelope.Algorithm != AlgorithmAES256GCM || envelope.KeyID == "" {
		return nil, ErrUnsupported
	}
	entry, found := r.keys[envelope.KeyID]
	if !found {
		return nil, ErrUnknownKey
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != entry.aead.NonceSize() {
		return nil, ErrMalformed
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil || len(ciphertext) < entry.aead.Overhead() {
		return nil, ErrMalformed
	}
	plaintext, err := entry.aead.Open(nil, nonce, ciphertext, binding.aad())
	if err != nil {
		return nil, fmt.Errorf("credential: authentication failed: %w", err)
	}
	return plaintext, nil
}

// OpenAndRotate only returns a replacement after authenticated old-key decryption.
func (r *Keyring) OpenAndRotate(envelope Envelope, binding Binding) ([]byte, *Envelope, error) {
	plaintext, err := r.Open(envelope, binding)
	if err != nil {
		return nil, nil, err
	}
	if envelope.KeyID == r.current {
		return plaintext, nil, nil
	}
	replacement, err := r.Seal(plaintext, binding)
	if err != nil {
		return nil, nil, err
	}
	return plaintext, &replacement, nil
}

func (b Binding) validate() error {
	if b.WorkspaceID == "" || b.CredentialID == "" || b.Purpose == "" {
		return ErrInvalidBinding
	}
	return nil
}

func (b Binding) aad() []byte {
	data, _ := json.Marshal(struct {
		WorkspaceID  string `json:"workspace_id"`
		CredentialID string `json:"credential_id"`
		Purpose      string `json:"purpose"`
	}{b.WorkspaceID, b.CredentialID, b.Purpose})
	return data
}
