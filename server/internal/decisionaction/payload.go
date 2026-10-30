package decisionaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// SlotTarget names the one place a secret value may land.
type SlotTarget struct {
	AgentID string `json:"agent_id"`
	EnvKey  string `json:"env_key"`
}

// SecretSlot is a declared input the human fills in on the card. The payload
// carries the slot name and target only, never a value.
type SecretSlot struct {
	Slot   string     `json:"slot"`
	Target SlotTarget `json:"target"`
}

// Payload is what the human approves and the passkey signs. Params and
// Preconditions are already-normalized JSON; ExpiresAt is the stored instant.
type Payload struct {
	ActionType        string
	ActionVersion     int
	WorkspaceID       string
	DecisionRequestID string
	Params            json.RawMessage
	Preconditions     json.RawMessage
	SecretSlots       []SecretSlot
	ExpiresAt         time.Time
}

// expiresAtLayout is fixed-width UTC at the microsecond precision Postgres
// stores, so a value read back from the database hashes the same.
const expiresAtLayout = "2006-01-02T15:04:05.000000Z"

func (p Payload) CanonicalJSON() ([]byte, error) {
	slots := p.SecretSlots
	if slots == nil {
		slots = []SecretSlot{}
	}
	raw, err := json.Marshal(struct {
		ActionType        string          `json:"action_type"`
		ActionVersion     int             `json:"action_version"`
		WorkspaceID       string          `json:"workspace_id"`
		DecisionRequestID string          `json:"decision_request_id"`
		Params            json.RawMessage `json:"params"`
		Preconditions     json.RawMessage `json:"preconditions"`
		SecretSlots       []SecretSlot    `json:"secret_slots"`
		ExpiresAt         string          `json:"expires_at"`
	}{
		p.ActionType, p.ActionVersion, p.WorkspaceID, p.DecisionRequestID,
		orEmptyObject(p.Params), orEmptyObject(p.Preconditions), slots,
		p.ExpiresAt.UTC().Format(expiresAtLayout),
	})
	if err != nil {
		return nil, err
	}
	return Canonical(raw)
}

// Hash is the lowercase hex SHA-256 of the canonical payload.
func (p Payload) Hash() (string, error) {
	canonical, err := p.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}
