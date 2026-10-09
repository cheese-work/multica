package taskgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/governance/credential"
)

const configurationLimit = 1 << 20

func operatorCredentialBinding() credential.Binding {
	return credential.Binding{WorkspaceID: "deployment", CredentialID: "task-gateway", Purpose: "operator-api"}
}

func DecodeConfiguration(contents []byte, ring *credential.Keyring) (*Provisioner, error) {
	if ring == nil || len(contents) == 0 || len(contents) > configurationLimit {
		return nil, ErrUnavailable
	}
	fields := json.NewDecoder(bytes.NewReader(contents))
	if validateJSONFields(fields, 0) != nil {
		return nil, ErrUnavailable
	}
	if _, err := fields.Token(); err != io.EOF {
		return nil, ErrUnavailable
	}
	var document struct {
		BaseURL            string              `json:"base_url"`
		OperatorCredential credential.Envelope `json:"operator_credential"`
		Policies           []Policy            `json:"policies"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || len(document.Policies) == 0 {
		return nil, ErrUnavailable
	}
	plaintext, err := ring.Open(document.OperatorCredential, operatorCredentialBinding())
	if err != nil {
		return nil, ErrUnavailable
	}
	defer clear(plaintext)
	return New(document.BaseURL, string(plaintext), document.Policies)
}

func LoadFromEnv() (*Provisioner, error) {
	path, encodedKey := os.Getenv("MULTICA_TASK_GATEWAY_CONFIG"), os.Getenv("MULTICA_TASK_GATEWAY_SECRET_KEY")
	if path == "" && encodedKey == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) || encodedKey == "" {
		return nil, ErrUnavailable
	}
	key, err := credential.DecodeKey("current", encodedKey)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer clear(key.Material)
	key.ID = credential.KeyID(key.Material)
	ring, err := credential.NewKeyring(key)
	if err != nil {
		return nil, ErrUnavailable
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > configurationLimit {
		return nil, ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrUnavailable
	}
	contents, err := io.ReadAll(io.LimitReader(file, configurationLimit+1))
	if err != nil {
		return nil, ErrUnavailable
	}
	return DecodeConfiguration(contents, ring)
}
