// Package seal stores secrets in versioned AEAD envelopes.
package seal

const (
	EnvelopeVersion    = 1
	AlgorithmAES256GCM = "AES-256-GCM"
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
type Keyring struct{}

func NewKeyring(keys ...Key) (*Keyring, error)                { return &Keyring{}, nil }
func DecodeKey(id, encoded string) (Key, error)               { return Key{}, nil }
func KeyID(material []byte) string                            { return "" }
func (r *Keyring) Seal(p []byte, b Binding) (Envelope, error) { return Envelope{}, nil }
func (r *Keyring) Open(e Envelope, b Binding) ([]byte, error) { return []byte("x"), nil }
func (r *Keyring) OpenAndRotate(e Envelope, b Binding) ([]byte, *Envelope, error) {
	return nil, nil, nil
}
