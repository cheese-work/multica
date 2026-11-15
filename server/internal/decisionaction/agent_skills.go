package decisionaction

import (
	"encoding/json"
	"slices"
)

const maxAgentSkills = 50

// AgentSkillsSetParams is agent.skills.set: replace one agent's skill list if
// the assignment still hashes to base_sha256. after_sha256 is the agent's claim
// about the new list; the server recomputes it.
type AgentSkillsSetParams struct {
	AgentID     string   `json:"agent_id"`
	SkillIDs    []string `json:"skill_ids"`
	BaseSHA256  string   `json:"base_sha256"`
	AfterSHA256 string   `json:"after_sha256"`
}

func normalizeAgentSkillsSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("agent.skills.set takes no secret slots")
	}
	var params AgentSkillsSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.AgentID, err = canonicalUUID("agent_id", params.AgentID); err != nil {
		return nil, nil, err
	}
	if params.SkillIDs == nil {
		return nil, nil, invalid("skill_ids is required; use [] to remove every skill")
	}
	ids := make([]string, 0, len(params.SkillIDs))
	for _, id := range params.SkillIDs {
		canonical, err := canonicalUUID("skill_ids", id)
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, canonical)
	}
	slices.Sort(ids)
	params.SkillIDs = slices.Compact(ids)
	if len(params.SkillIDs) > maxAgentSkills {
		return nil, nil, invalid("skill_ids holds at most 50 distinct skills")
	}
	if !digestPattern.MatchString(params.BaseSHA256) || !digestPattern.MatchString(params.AfterSHA256) {
		return nil, nil, invalid("base_sha256 and after_sha256 must be lowercase hex sha256 digests")
	}
	out, err := json.Marshal(params)
	return out, nil, err
}
