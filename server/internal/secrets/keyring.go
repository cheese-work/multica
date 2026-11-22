package secrets

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/multica-ai/multica/server/internal/secrets/seal"
)

// Environment variables holding the scoped-secrets keyring (base64 of 32 bytes).
const (
	EnvKey         = "MULTICA_SECRETS_KEY"
	EnvKeyPrevious = "MULTICA_SECRETS_KEY_PREVIOUS"
)

// ErrMalformedKey is returned when a configured key is not base64 of 32 bytes.
// It never carries key material.
var ErrMalformedKey = errors.New("secrets key is malformed: value is not base64 of 32 bytes")

// KeyStatus says how the scoped-secrets key was configured.
type KeyStatus string

const (
	KeyUnset     KeyStatus = "unset"
	KeyMalformed KeyStatus = "malformed"
	KeyOK        KeyStatus = "ok"
)

// LoadKeyring builds the scoped-secrets keyring from the environment. The
// current key seals new envelopes; an optional previous key only opens old ones.
// The ring is nil unless the status is KeyOK. Key values are never logged or
// returned in errors.
func LoadKeyring(getenv func(string) string) (*seal.Keyring, KeyStatus, error) {
	raw := strings.TrimSpace(getenv(EnvKey))
	if raw == "" {
		return nil, KeyUnset, nil
	}
	current, err := seal.DecodeKey("current", raw)
	if err != nil {
		return nil, KeyMalformed, ErrMalformedKey
	}
	current.ID = seal.KeyID(current.Material)
	keys := []seal.Key{current}
	if prevRaw := strings.TrimSpace(getenv(EnvKeyPrevious)); prevRaw != "" {
		prev, err := seal.DecodeKey("previous", prevRaw)
		if err != nil {
			return nil, KeyMalformed, ErrMalformedKey
		}
		prev.ID = seal.KeyID(prev.Material)
		if prev.ID != current.ID {
			keys = append(keys, prev)
		}
	}
	ring, err := seal.NewKeyring(keys...)
	if err != nil {
		return nil, KeyMalformed, ErrMalformedKey
	}
	return ring, KeyOK, nil
}

// LogKeyStatus logs the outcome once. Outside production a missing or bad key
// only disables the feature.
func LogKeyStatus(logger *slog.Logger, status KeyStatus) {
	switch status {
	case KeyOK:
		logger.Info("scoped secrets enabled")
	default:
		logger.Warn("scoped secrets disabled", "reason", string(status))
	}
}
