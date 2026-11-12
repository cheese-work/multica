package decisionaction

import "encoding/json"

// AgentSchedulingParams is agent.pause and agent.resume: stop or restart one
// agent's claims. The change is reversible and carries no secret.
type AgentSchedulingParams struct {
	AgentID string `json:"agent_id"`
}

func normalizeAgentSchedulingChange(actionType string) func(json.RawMessage, []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	return func(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
		if len(slots) > 0 {
			return nil, nil, invalid("%s takes no secret slots", actionType)
		}
		var params AgentSchedulingParams
		if err := decodeStrict(raw, &params); err != nil {
			return nil, nil, err
		}
		var err error
		if params.AgentID, err = canonicalUUID("agent_id", params.AgentID); err != nil {
			return nil, nil, err
		}
		out, err := json.Marshal(params)
		return out, nil, err
	}
}
