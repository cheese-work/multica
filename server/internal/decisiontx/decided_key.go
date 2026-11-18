package decisiontx

import (
	"encoding/json"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// KeyVersion prefixes every decided-ask key document. A change to the key
// rules bumps it, so keys of two versions never compare equal.
const KeyVersion = "v1"

// KeyExcludedAskFields names, by JSON field, the only Ask fields left out of
// the match key. Every other Ask field is in the key.
var KeyExcludedAskFields = []string{}

// ActionKeyInput is the action of a decision request or proposal: the new
// draft, or the stored decision_action row. Params is the raw JSON object.
type ActionKeyInput struct {
	Type        string
	Version     int
	Params      json.RawMessage
	SecretSlots []decisionaction.SecretSlot
}

// SubjectKey groups asks of one subject for supersession.
func SubjectKey(kind string, ask Ask) (string, error) { return "", nil }

// MatchKey is the key two asks share only when they ask the same thing.
func MatchKey(kind, escalationClass string, ask Ask, action *ActionKeyInput) (string, error) {
	return "", nil
}

// ProposalKey is the subject key and the match key of a decision proposal.
func ProposalKey(action ActionKeyInput) (string, error) { return "", nil }

// matchKeyDoc is the canonical JSON the match key hashes.
func matchKeyDoc(kind, escalationClass string, ask Ask, action *ActionKeyInput) ([]byte, error) {
	return nil, nil
}
