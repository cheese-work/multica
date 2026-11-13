package decisionaction

import (
	"encoding/json"
	"regexp"
	"strings"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AgentInstructionsSetParams is agent.instructions.set: replace one agent's
// instructions if they still hash to base_sha256. after_sha256 is the agent's
// claim about the new text; the server recomputes it.
type AgentInstructionsSetParams struct {
	AgentID      string  `json:"agent_id"`
	Instructions *string `json:"instructions"`
	BaseSHA256   string  `json:"base_sha256"`
	AfterSHA256  string  `json:"after_sha256"`
}

func normalizeAgentInstructionsSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("agent.instructions.set takes no secret slots")
	}
	var params AgentInstructionsSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.AgentID, err = canonicalUUID("agent_id", params.AgentID); err != nil {
		return nil, nil, err
	}
	if params.Instructions == nil {
		return nil, nil, invalid("instructions is required")
	}
	if strings.ContainsRune(*params.Instructions, 0) {
		return nil, nil, invalid("instructions must not contain a NUL byte")
	}
	if !digestPattern.MatchString(params.BaseSHA256) || !digestPattern.MatchString(params.AfterSHA256) {
		return nil, nil, invalid("base_sha256 and after_sha256 must be lowercase hex sha256 digests")
	}
	out, err := json.Marshal(params)
	return out, nil, err
}
