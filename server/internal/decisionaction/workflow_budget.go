package decisionaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// WorkflowBudgetSet changes a workflow's run budget: its per-issue run cap and
// whether child-PR runs count against it (CHE-5228). Either field may be left
// out to keep the stored value; a cap of 0 clears it to the platform default.
const WorkflowBudgetSet = "workflow.budget.set"

type WorkflowBudgetSetParams struct {
	WorkflowID      string `json:"workflow_id"`
	BaseSHA256      string `json:"base_sha256"`
	MaxRunsPerIssue *int   `json:"max_runs_per_issue,omitempty"`
	IncludeChildPRs *bool  `json:"include_child_prs,omitempty"`
}

func init() {
	// Spend class: the target is a workflow, so only the owner approves it.
	Register(&Definition{Type: WorkflowBudgetSet, Version: 1, Template: Template{Title: "Change a workflow's run budget", Summary: "Changes the workflow's per-issue run cap or whether child-PR runs count against it."}, MinClass: "spend", Target: TargetWorkflow, RequiresPreview: true, Normalize: normalizeWorkflowBudgetSet})
}

// WorkflowBudgetDigest is the digest of a workflow's budget: sha256 of the
// canonical JSON of {"include_child_prs", "max_runs_per_issue", "workflow_id"}
// with keys in that order. An unset cap digests as null.
func WorkflowBudgetDigest(workflowID string, maxRuns *int, includeChildPRs bool) string {
	canonical, _ := json.Marshal(struct {
		IncludeChildPRs bool   `json:"include_child_prs"`
		MaxRunsPerIssue *int   `json:"max_runs_per_issue"`
		WorkflowID      string `json:"workflow_id"`
	}{includeChildPRs, maxRuns, workflowID})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func normalizeWorkflowBudgetSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("workflow.budget.set takes no secret slots")
	}
	var params WorkflowBudgetSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.WorkflowID, err = canonicalUUID("workflow_id", params.WorkflowID); err != nil {
		return nil, nil, err
	}
	if !sha256Hex.MatchString(params.BaseSHA256) {
		return nil, nil, invalid("base_sha256 must be a lowercase hex sha256")
	}
	if params.MaxRunsPerIssue == nil && params.IncludeChildPRs == nil {
		return nil, nil, invalid("name max_runs_per_issue, include_child_prs or both")
	}
	if cap := params.MaxRunsPerIssue; cap != nil && (*cap < 0 || *cap > issueworkflow.MaxRunsPerIssueLimit) {
		return nil, nil, invalid("max_runs_per_issue must be 0 to the platform limit")
	}
	out, err := json.Marshal(params)
	return out, nil, err
}
