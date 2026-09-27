package credential

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// LoadKeyringFromEnv reads a keyring from envVar, formatted as
// comma-separated "keyID:base64key" entries, ordered newest (primary)
// first — e.g. "k2:<base64>,k1:<base64>" makes k2 the key new Seal calls
// use while k1 remains available to Open envelopes sealed before rotation.
//
// An unset or blank env var returns (nil, nil): "not configured" is a
// normal, safe state (mirrors [secretbox.LoadKey]'s per-integration
// pattern) — the server starts, credential writes/reads for this purpose
// return a clear "not configured" error, and nothing else is affected.
// A *present but malformed* value is a configuration error and fails loudly
// instead of silently disabling encryption.
func LoadKeyringFromEnv(envVar string) (*Keyring, error) {
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		return nil, nil
	}
	entries := strings.Split(raw, ",")
	keys := make([]VersionedKey, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		id, encoded, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("credential: %s entry %q is not \"keyID:base64key\"", envVar, entry)
		}
		id = strings.TrimSpace(id)
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("credential: %s entry %q: invalid base64: %w", envVar, id, err)
		}
		keys = append(keys, VersionedKey{ID: id, Key: key})
	}
	kr, err := NewKeyring(keys)
	if err != nil {
		return nil, fmt.Errorf("credential: %s: %w", envVar, err)
	}
	return kr, nil
}
