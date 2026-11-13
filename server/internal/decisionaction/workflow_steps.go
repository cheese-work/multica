package decisionaction

import (
	"encoding/json"
	"regexp"
)

// WorkflowStepsSet replaces the steps of one workflow, instructions included
// (CHE-3348). The agent sends the whole steps array as GET returns it, plus the
// digests of the live and of the resulting workflow.
const WorkflowStepsSet = "workflow.steps.set"

type WorkflowStepsSetParams struct {
	WorkflowID  string          `json:"workflow_id"`
	Steps       json.RawMessage `json:"steps"`
	BaseSHA256  string          `json:"base_sha256"`
	AfterSHA256 string          `json:"after_sha256"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func init() {
	// Spend class: the target is a workflow, so only the owner approves it.
	Register(&Definition{Type: WorkflowStepsSet, Version: 1, Template: Template{Title: "Replace a workflow's steps", Summary: "Replaces the workflow's step list with the proposed steps."}, MinClass: "spend", Target: TargetWorkflow, RequiresPreview: true, Normalize: normalizeWorkflowStepsSet})
}

func normalizeWorkflowStepsSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("workflow.steps.set takes no secret slots")
	}
	var params WorkflowStepsSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.WorkflowID, err = canonicalUUID("workflow_id", params.WorkflowID); err != nil {
		return nil, nil, err
	}
	var steps []json.RawMessage
	if json.Unmarshal(params.Steps, &steps) != nil || len(steps) == 0 {
		return nil, nil, invalid("steps must be a non-empty array")
	}
	if !sha256Hex.MatchString(params.BaseSHA256) || !sha256Hex.MatchString(params.AfterSHA256) {
		return nil, nil, invalid("base_sha256 and after_sha256 must be lowercase hex sha256")
	}
	out, err := json.Marshal(params)
	return out, nil, err
}
