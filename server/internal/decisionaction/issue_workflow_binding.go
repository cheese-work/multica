package decisionaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// IssueWorkflowField is the governed field of a decision-bound write that
// widens the run budget an issue follows: a workflow override on an issue, or a
// wider workflow named at create (CHE-5228).
const IssueWorkflowField = "issue.workflow"

// IssueWorkflowDigest is the digest of an issue's stored workflow binding:
// sha256 of the canonical JSON of {"issue_id", "project_id", "workflow_id"}
// with keys in that order. An empty argument digests as null: no issue yet on a
// create, no project, or no override (the issue follows its project's workflow).
func IssueWorkflowDigest(issueID, projectID, workflowID string) string {
	nullable := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	canonical, _ := json.Marshal(struct {
		IssueID    *string `json:"issue_id"`
		ProjectID  *string `json:"project_id"`
		WorkflowID *string `json:"workflow_id"`
	}{nullable(issueID), nullable(projectID), nullable(workflowID)})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
