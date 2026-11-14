package decisionaction

import (
	"encoding/json"
	"time"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// Binding sources: where an issue's effective workflow comes from.
const (
	WorkflowSourceIssue   = "issue"
	WorkflowSourceProject = "project"
	WorkflowSourceNone    = "none"
)

// IssueWorkflowSetBinding is the run budget an issue gets from the workflow it
// follows, as the person saw it. The executor recomputes it under the locks and
// compares every field.
type IssueWorkflowSetBinding struct {
	// WorkflowID is the effective workflow; empty for the built-in Default.
	WorkflowID string `json:"workflow_id"`
	// Source is issue, project or none.
	Source string `json:"source"`
	// ProjectID and ProjectWorkflowID are set when the issue is in a project;
	// the project's workflow id is empty when the project has none.
	ProjectID         string `json:"project_id,omitempty"`
	ProjectWorkflowID string `json:"project_workflow_id,omitempty"`
	Budgeted          bool   `json:"budgeted"`
	Cap               int    `json:"cap"`
	// Start is the budget window start, RFC3339Nano UTC; empty when unbudgeted.
	Start string `json:"start,omitempty"`
}

// IssueWorkflowSetParams is the issue.workflow.set payload. WorkflowID is the
// target (nil returns the issue to its project's workflow); ExplicitWorkflowID
// is the issue's own override when the card was opened.
type IssueWorkflowSetParams struct {
	IssueID            string                  `json:"issue_id"`
	WorkflowID         *string                 `json:"workflow_id"`
	ExplicitWorkflowID *string                 `json:"explicit_workflow_id"`
	Before             IssueWorkflowSetBinding `json:"before"`
	After              IssueWorkflowSetBinding `json:"after"`
}

func normalizeIssueWorkflowSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("issue.workflow.set takes no secret slots")
	}
	var params IssueWorkflowSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.IssueID, err = canonicalUUID("issue_id", params.IssueID); err != nil {
		return nil, nil, err
	}
	for field, id := range map[string]**string{"workflow_id": &params.WorkflowID, "explicit_workflow_id": &params.ExplicitWorkflowID} {
		if *id == nil {
			continue
		}
		canonical, err := canonicalUUID(field, **id)
		if err != nil {
			return nil, nil, err
		}
		*id = &canonical
	}
	if err := normalizeWorkflowSetBinding("before", &params.Before); err != nil {
		return nil, nil, err
	}
	if err := normalizeWorkflowSetBinding("after", &params.After); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

func normalizeWorkflowSetBinding(name string, b *IssueWorkflowSetBinding) error {
	var err error
	for field, id := range map[string]*string{"workflow_id": &b.WorkflowID, "project_id": &b.ProjectID, "project_workflow_id": &b.ProjectWorkflowID} {
		if *id == "" {
			continue
		}
		if *id, err = canonicalUUID(name+"."+field, *id); err != nil {
			return err
		}
	}
	switch b.Source {
	case WorkflowSourceIssue, WorkflowSourceProject, WorkflowSourceNone:
	default:
		return invalid("%s.source must be issue, project or none", name)
	}
	if b.Budgeted {
		if b.Cap < 1 || b.Cap > issueworkflow.MaxRunsPerIssueLimit {
			return invalid("%s.cap must be 1-%d", name, issueworkflow.MaxRunsPerIssueLimit)
		}
		at, err := time.Parse(time.RFC3339Nano, b.Start)
		if err != nil || at.UTC().Format(time.RFC3339Nano) != b.Start {
			return invalid("%s.start must be an RFC3339 UTC time", name)
		}
	} else if b.Cap != 0 || b.Start != "" {
		return invalid("%s is unbudgeted: cap and start must be empty", name)
	}
	return nil
}
