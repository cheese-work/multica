package decisionaction

import "encoding/json"

// WorkflowStepInstructionsSet replaces the instructions of one workflow step
// (CHE-2949 D5b). The agent names the step and the digest of the workflow it
// read; the server computes the rest.
const WorkflowStepInstructionsSet = "workflow.step.instructions.set"

type WorkflowStepInstructionsSetParams struct {
	WorkflowID   string `json:"workflow_id"`
	StatusKey    string `json:"status_key"`
	BaseSHA256   string `json:"base_sha256"`
	Instructions string `json:"instructions"`
}

func init() {
	// Spend class: the target is a workflow, so only the owner approves it.
	Register(&Definition{Type: WorkflowStepInstructionsSet, Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Replace a workflow step's instructions", Summary: "Replaces the instructions of one workflow step with the proposed text."}, MinClass: "spend", Target: TargetWorkflow, RequiresPreview: true, Normalize: normalizeWorkflowStepInstructionsSet})
}

func normalizeWorkflowStepInstructionsSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("workflow.step.instructions.set takes no secret slots")
	}
	var params WorkflowStepInstructionsSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.WorkflowID, err = canonicalUUID("workflow_id", params.WorkflowID); err != nil {
		return nil, nil, err
	}
	if params.StatusKey == "" {
		return nil, nil, invalid("status_key must not be empty")
	}
	if !sha256Hex.MatchString(params.BaseSHA256) {
		return nil, nil, invalid("base_sha256 must be a lowercase hex sha256")
	}
	out, err := json.Marshal(params)
	return out, nil, err
}
