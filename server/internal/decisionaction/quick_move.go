package decisionaction

import (
	"encoding/json"
	"sort"
)

// Owner-approval actions for an agent's move into a Quick binding (CHE-7734).
// Both seal the binding the person saw; the executor recomputes it under the
// locks and ends the action as stale_base on any difference.

const (
	// IssueProjectQuickSet moves one issue into a project that follows Quick.
	IssueProjectQuickSet = "issue.project.quick_set"
	// ProjectWorkflowQuickSet switches a project to Quick and remaps its issues.
	ProjectWorkflowQuickSet = "project.workflow.quick_set"
	// ProjectWorkflowSet switches many projects to one workflow when the switch
	// widens a run budget, and remaps their issues, all or none (CHE-10137).
	ProjectWorkflowSet = "project.workflow.set"
	// MaxProjectWorkflowSetProjects caps the projects one card covers.
	MaxProjectWorkflowSetProjects = 50
)

func init() {
	Register(&Definition{Type: IssueProjectQuickSet, Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Move an issue into a Quick project", Summary: "Moves the issue to a project that follows the Quick workflow, which gives it the Quick steps and run budget."}, MinClass: "spend", Target: TargetIssue, Normalize: normalizeIssueProjectQuickSet})
	Register(&Definition{Type: ProjectWorkflowQuickSet, Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Switch a project to Quick", Summary: "Switches the project to the Quick workflow and moves its issues to the statuses listed."}, MinClass: "spend", Target: TargetNone, Normalize: normalizeProjectWorkflowQuickSet})
	Register(&Definition{Type: ProjectWorkflowSet, Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Switch projects to a workflow", Summary: "Switches each listed project to the workflow and moves its issues to the statuses listed. Every project changes, or none does."}, MinClass: "spend", Target: TargetNone, Normalize: normalizeProjectWorkflowSet})
}

// IssueProjectQuickParams is the issue.project.quick_set payload. IssueID and
// ProjectID are the same fields issue.project.set carries. ExplicitWorkflowID is
// the issue's own override when the card was opened; Before and After seal the
// binding the issue has now and the one it gets in ProjectID.
type IssueProjectQuickParams struct {
	IssueID            string                  `json:"issue_id"`
	ProjectID          string                  `json:"project_id"`
	ExplicitWorkflowID *string                 `json:"explicit_workflow_id"`
	Before             IssueWorkflowSetBinding `json:"before"`
	After              IssueWorkflowSetBinding `json:"after"`
}

// StatusCount is the number of issues a project has on one status.
type StatusCount struct {
	StatusKey  string `json:"status_key"`
	IssueCount int64  `json:"issue_count"`
}

// ProjectWorkflowQuickParams is the project.workflow.quick_set payload.
// WorkflowID is the target workflow. Required lists the statuses the target
// lacks with the issue count each had when the card was opened, and
// StatusMapping says where each goes. Before and After seal the project's
// binding, with the smallest run override among the issues that follow it.
// Overrides lists every distinct run override (0 for none) among those issues,
// sorted: each one sets its issues' effective cap before and after the switch.
type ProjectWorkflowQuickParams struct {
	ProjectID     string                  `json:"project_id"`
	WorkflowID    *string                 `json:"workflow_id"`
	StatusMapping map[string]string       `json:"status_mapping"`
	Required      []StatusCount           `json:"required"`
	TotalIssues   int64                   `json:"total_issues"`
	Overrides     []int32                 `json:"overrides"`
	Before        IssueWorkflowSetBinding `json:"before"`
	After         IssueWorkflowSetBinding `json:"after"`
}

func normalizeIssueProjectQuickSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("%s takes no secret slots", IssueProjectQuickSet)
	}
	var params IssueProjectQuickParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.IssueID, err = canonicalUUID("issue_id", params.IssueID); err != nil {
		return nil, nil, err
	}
	if params.ProjectID, err = canonicalUUID("project_id", params.ProjectID); err != nil {
		return nil, nil, err
	}
	if params.ExplicitWorkflowID != nil {
		canonical, err := canonicalUUID("explicit_workflow_id", *params.ExplicitWorkflowID)
		if err != nil {
			return nil, nil, err
		}
		params.ExplicitWorkflowID = &canonical
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

func normalizeProjectWorkflowQuickSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("%s takes no secret slots", ProjectWorkflowQuickSet)
	}
	var params ProjectWorkflowQuickParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	if err := normalizeProjectSwitch(&params); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

// normalizeProjectSwitch canonicalizes one project's sealed switch in place.
func normalizeProjectSwitch(params *ProjectWorkflowQuickParams) error {
	var err error
	if params.ProjectID, err = canonicalUUID("project_id", params.ProjectID); err != nil {
		return err
	}
	if params.WorkflowID != nil {
		canonical, err := canonicalUUID("workflow_id", *params.WorkflowID)
		if err != nil {
			return err
		}
		params.WorkflowID = &canonical
	}
	if params.TotalIssues < 0 {
		return invalid("total_issues must not be negative")
	}
	if params.StatusMapping == nil {
		params.StatusMapping = map[string]string{}
	}
	// The card seals the statuses that move: sorted, so the same switch always
	// hashes the same.
	if params.Required == nil {
		params.Required = []StatusCount{}
	}
	sort.Slice(params.Required, func(i, j int) bool { return params.Required[i].StatusKey < params.Required[j].StatusKey })
	for _, required := range params.Required {
		if required.StatusKey == "" || required.IssueCount < 1 {
			return invalid("required needs a status key and a positive issue count")
		}
		if params.StatusMapping[required.StatusKey] == "" {
			return invalid("status_mapping has no destination for %q", required.StatusKey)
		}
	}
	if params.Overrides == nil {
		params.Overrides = []int32{}
	}
	sort.Slice(params.Overrides, func(i, j int) bool { return params.Overrides[i] < params.Overrides[j] })
	for _, override := range params.Overrides {
		if override < 0 {
			return invalid("overrides must not be negative")
		}
	}
	if err := normalizeWorkflowSetBinding("before", &params.Before); err != nil {
		return err
	}
	return normalizeWorkflowSetBinding("after", &params.After)
}

// ProjectWorkflowSetParams is the project.workflow.set payload: one target
// workflow for 1 to MaxProjectWorkflowSetProjects projects. Each entry seals
// the project, the workflow it follows now (Before.ProjectWorkflowID), the
// budget binding before and after, and the status mapping, as quick_set does.
// Every entry's WorkflowID is the top-level one.
type ProjectWorkflowSetParams struct {
	WorkflowID *string                      `json:"workflow_id"`
	Projects   []ProjectWorkflowQuickParams `json:"projects"`
}

func normalizeProjectWorkflowSet(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("%s takes no secret slots", ProjectWorkflowSet)
	}
	var params ProjectWorkflowSetParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	if params.WorkflowID != nil {
		canonical, err := canonicalUUID("workflow_id", *params.WorkflowID)
		if err != nil {
			return nil, nil, err
		}
		params.WorkflowID = &canonical
	}
	if n := len(params.Projects); n < 1 || n > MaxProjectWorkflowSetProjects {
		return nil, nil, invalid("projects takes 1 to %d entries, got %d", MaxProjectWorkflowSetProjects, n)
	}
	seen := map[string]bool{}
	for i := range params.Projects {
		entry := &params.Projects[i]
		if err := normalizeProjectSwitch(entry); err != nil {
			return nil, nil, err
		}
		if seen[entry.ProjectID] {
			return nil, nil, invalid("project %s is listed twice", entry.ProjectID)
		}
		seen[entry.ProjectID] = true
		if (entry.WorkflowID == nil) != (params.WorkflowID == nil) || (entry.WorkflowID != nil && *entry.WorkflowID != *params.WorkflowID) {
			return nil, nil, invalid("every project entry must carry the target workflow_id")
		}
	}
	// Sorted by project, so the same set always hashes the same and the
	// approval takes its locks in one order.
	sort.Slice(params.Projects, func(i, j int) bool { return params.Projects[i].ProjectID < params.Projects[j].ProjectID })
	out, err := json.Marshal(params)
	return out, nil, err
}
