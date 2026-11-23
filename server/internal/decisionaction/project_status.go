package decisionaction

import "encoding/json"

// ProjectStatusSet is the action an agent proposes to change a project's
// status (CHE-9125). An agent's direct status write is refused.
const ProjectStatusSet = "project.status.set"

// ProjectStatusSetParams is the project.status.set payload.
type ProjectStatusSetParams struct {
	ProjectID string `json:"project_id"`
	Status    string `json:"status"`
}

var projectStatuses = map[string]bool{"planned": true, "in_progress": true, "paused": true, "completed": true, "cancelled": true}

func init() {
	Register(&Definition{Type: ProjectStatusSet, Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Change a project's status", Summary: "Sets the project to the proposed status. A paused, completed or cancelled project stops starting runs."}, MinClass: "reversible", Target: TargetNone, Normalize: normalizeProjectStatusSet})
}

func normalizeProjectStatusSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("%s takes no secret slots", ProjectStatusSet)
	}
	var params ProjectStatusSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.ProjectID, err = canonicalUUID("project_id", params.ProjectID); err != nil {
		return nil, nil, err
	}
	if !projectStatuses[params.Status] {
		return nil, nil, invalid("status must be one of planned, in_progress, paused, completed, cancelled")
	}
	out, err := json.Marshal(params)
	return out, nil, err
}
