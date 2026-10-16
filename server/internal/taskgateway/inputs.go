package taskgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

const InputSnapshotLimit = 3 << 20

type InputSnapshot struct {
	Binding          credentialexec.Binding `json:"binding"`
	RuntimeID        string                 `json:"runtime_id"`
	AgentID          string                 `json:"agent_id"`
	DispatchedAt     string                 `json:"dispatched_at"`
	Instructions     string                 `json:"instructions"`
	WorkspaceContext string                 `json:"workspace_context"`
}

func (snapshot InputSnapshot) Validate() error {
	if snapshot.Binding.Validate() != nil || len(snapshot.Instructions) > 1<<20 || len(snapshot.WorkspaceContext) > 1<<20 || !utf8.ValidString(snapshot.Instructions) || !utf8.ValidString(snapshot.WorkspaceContext) {
		return ErrUnavailable
	}
	for _, value := range []string{snapshot.RuntimeID, snapshot.AgentID} {
		identity, err := uuid.Parse(value)
		if err != nil || identity == uuid.Nil || identity.String() != value {
			return ErrUnavailable
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, snapshot.DispatchedAt); err != nil {
		return ErrUnavailable
	}
	return nil
}

func EncodeInputSnapshot(snapshot InputSnapshot) ([]byte, error) {
	if snapshot.Validate() != nil {
		return nil, ErrUnavailable
	}
	contents, err := json.Marshal(snapshot)
	if err != nil || len(contents) > InputSnapshotLimit {
		return nil, ErrUnavailable
	}
	return contents, nil
}

func DecodeInputSnapshot(contents []byte) (InputSnapshot, error) {
	if len(contents) == 0 || len(contents) > InputSnapshotLimit || !utf8.Valid(contents) {
		return InputSnapshot{}, ErrUnavailable
	}
	fields := json.NewDecoder(bytes.NewReader(contents))
	if validateJSONFields(fields, 0) != nil {
		return InputSnapshot{}, ErrUnavailable
	}
	if _, err := fields.Token(); err != io.EOF {
		return InputSnapshot{}, ErrUnavailable
	}
	var required map[string]json.RawMessage
	if json.Unmarshal(contents, &required) != nil || len(required["instructions"]) == 0 || bytes.Equal(required["instructions"], []byte("null")) || len(required["workspace_context"]) == 0 || bytes.Equal(required["workspace_context"], []byte("null")) {
		return InputSnapshot{}, ErrUnavailable
	}
	var snapshot InputSnapshot
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || snapshot.Validate() != nil {
		return InputSnapshot{}, ErrUnavailable
	}
	return snapshot, nil
}
