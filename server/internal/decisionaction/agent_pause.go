package decisionaction

import "encoding/json"

// AgentPauseParams is agent.pause and agent.resume: flip one agent's
// scheduling pause. Running work finishes; queued work waits.
type AgentPauseParams struct {
	AgentID string `json:"agent_id"`
}

func normalizeAgentPause(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("agent.pause and agent.resume take no secret slots")
	}
	var params AgentPauseParams
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
