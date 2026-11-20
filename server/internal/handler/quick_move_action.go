package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Owner approval for an agent's move into Quick (CHE-7734). An agent run that
// moves an issue into a project following Quick, or switches a project to
// Quick, opens a card instead of failing. Each card seals the binding before
// and after; the executor recomputes both under the locks and ends the action
// as stale_base when anything differs.

// runIssue is the issue the request's task runs on, the host of a card that
// has no issue of its own.
// Literals, not decisionaction constants: the preview contract test resolves
// only string literals in this package.
const (
	issueProjectQuickSetType    = "issue.project.quick_set"
	projectWorkflowQuickSetType = "project.workflow.quick_set"
)

func (h *Handler) runIssue(r *http.Request) (db.Issue, bool) {
	task, ok := h.taskFromRequestHeader(r)
	if !ok || !task.IssueID.Valid {
		return db.Issue{}, false
	}
	issue, err := h.Queries.GetIssue(r.Context(), task.IssueID)
	if err != nil || uuidToString(issue.WorkspaceID) != h.resolveWorkspaceID(r) {
		return db.Issue{}, false
	}
	return issue, true
}

// issueProjectQuickParams seals the move of issue into projectID.
func issueProjectQuickParams(ctx context.Context, q issueworkflow.Querier, issue db.Issue, projectID pgtype.UUID) (decisionaction.IssueProjectQuickParams, error) {
	before, _, _, err := sealWorkflowBinding(ctx, q, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID, issue.MaxRunsOverride)
	if err != nil {
		return decisionaction.IssueProjectQuickParams{}, err
	}
	after, _, _, err := sealWorkflowBinding(ctx, q, issue.WorkspaceID, issue.WorkflowID, projectID, issue.MaxRunsOverride)
	if err != nil {
		return decisionaction.IssueProjectQuickParams{}, err
	}
	return decisionaction.IssueProjectQuickParams{
		IssueID: uuidToString(issue.ID), ProjectID: uuidToString(projectID), ExplicitWorkflowID: uuidStringPtr(issue.WorkflowID),
		Before: before, After: after,
	}, nil
}

// writeQuickMoveCard answers an agent's `issue update --project` into Quick
// with the owners' card. The card moves the project alone: another field in the
// same request would be dropped silently, so it is refused.
func (h *Handler) writeQuickMoveCard(w http.ResponseWriter, r *http.Request, approval quickMoveApproval, rawFields map[string]json.RawMessage) {
	ctx := r.Context()
	if len(rawFields) != 1 {
		writeErrorCode(w, http.StatusConflict, "quick_move_alone", "a move into a Quick project asks the owners to approve it; send project_id alone, and make other changes in a separate update")
		return
	}
	issue, err := h.Queries.GetIssue(ctx, approval.issueID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update issue")
		return
	}
	target, err := h.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: approval.projectID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update issue")
		return
	}
	params, err := issueProjectQuickParams(ctx, h.Queries, issue, target.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update issue")
		return
	}
	ask := fmt.Sprintf("**Quick move request**\nAn agent asked to move [Issue](mention://issue/%s) into the Quick project %q (run cap %s to %s).\nApprove it with your passkey in the web app, or reject it.",
		uuidToString(issue.ID), target.Title, workflowSetCapLabel(params.Before), workflowSetCapLabel(params.After))
	opened, err := h.openOwnerCard(ctx, issue, ownerCard{actionType: decisionaction.IssueProjectQuickSet, name: "issue_project_quick_set", ask: ask, params: params})
	if err != nil {
		writeDecisionError(w, err, "issue project request")
		return
	}
	writeJSON(w, http.StatusAccepted, opened)
}

// executeIssueProjectQuickSetAction moves the issue under the locks the
// approval holds: the issue's boundary, the catalog (shared), both projects and
// the issue row.
func executeIssueProjectQuickSetAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.IssueProjectQuickParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return nil, err
	}
	queries := execution.Queries
	issue, err := queries.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issueID, WorkspaceID: execution.Action.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, staleBase("the issue no longer exists")
	}
	if err != nil {
		return nil, err
	}
	// The plan locked the project the issue was in when it read it.
	if issue.ProjectID != execution.ProjectID {
		return nil, staleBase("the issue moved to another project since the request; ask again")
	}
	move, err := loadIssueProjectMove(ctx, queries, issue, execution.Action.Params)
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		return nil, staleBase("the target project or its workflow changed since the request; ask again")
	}
	if err != nil {
		return nil, err
	}
	if err := service.RequireActiveProject(ctx, queries, issue.WorkspaceID, move.target.ID); errors.Is(err, service.ErrProjectInactive) {
		return nil, actionFailure{Code: "project_inactive", Message: "the target project is not active"}
	} else if err != nil {
		return nil, err
	}
	current, err := issueProjectQuickParams(ctx, queries, issue, move.target.ID)
	if err != nil {
		return nil, err
	}
	if !sameOptionalID(current.ExplicitWorkflowID, params.ExplicitWorkflowID) {
		return nil, staleBase("the issue's own workflow changed since the request; ask again")
	}
	if current.Before != params.Before || current.After != params.After {
		return nil, staleBase("the run budget of the issue or the target project changed since the request; ask again")
	}
	result, err := applyIssueProjectMove(ctx, execution, issue, move, projectIDString(issue.ProjectID))
	if err != nil {
		return nil, err
	}
	if move.toWorkflow != nil {
		if err := recordQuickEntryOnBind(ctx, queries, issue, move.toWorkflow, "", pgtype.UUID{}); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// projectBinding seals the binding a project gives its issues under def, the
// workflow it follows (nil is none), with the smallest run override among them.
func projectBinding(project db.Project, workflowID pgtype.UUID, def *issueworkflow.Definition, override pgtype.Int4) decisionaction.IssueWorkflowSetBinding {
	sealed := decisionaction.IssueWorkflowSetBinding{Source: decisionaction.WorkflowSourceProject, ProjectID: uuidToString(project.ID)}
	if workflowID.Valid {
		sealed.ProjectWorkflowID = uuidToString(workflowID)
	}
	if def != nil && def.ID.Valid {
		sealed.WorkflowID = uuidToString(def.ID)
	}
	if binding := workflowBinding(def, override); binding.budgeted {
		sealed.Budgeted, sealed.Cap = true, binding.cap
		sealed.Start = binding.start.UTC().Format(time.RFC3339Nano)
	}
	return sealed
}

// projectWorkflowQuickParams seals the switch of sw's project to target.
func projectWorkflowQuickParams(ctx context.Context, q *db.Queries, sw projectSwitchPlan, target *issueworkflow.Definition, targetID pgtype.UUID, mapping map[string]string) (decisionaction.ProjectWorkflowQuickParams, error) {
	current, next, override, err := projectSwitchBindings(ctx, q, sw.project, target)
	if err != nil {
		return decisionaction.ProjectWorkflowQuickParams{}, err
	}
	params := decisionaction.ProjectWorkflowQuickParams{
		ProjectID: uuidToString(sw.project.ID), WorkflowID: uuidStringPtr(targetID),
		StatusMapping: map[string]string{}, Required: []decisionaction.StatusCount{}, TotalIssues: sw.plan.TotalIssues,
		Before: projectBinding(sw.project, sw.project.WorkflowID, current, override),
		After:  projectBinding(sw.project, targetID, next, override),
	}
	for _, required := range sw.plan.Required {
		params.Required = append(params.Required, decisionaction.StatusCount{StatusKey: required.StatusKey, IssueCount: required.IssueCount})
		params.StatusMapping[required.StatusKey] = mapping[required.StatusKey]
	}
	return params, nil
}

func (h *Handler) openProjectWorkflowQuickCard(ctx context.Context, host db.Issue, project db.Project, params decisionaction.ProjectWorkflowQuickParams) (RunBudgetActionResponse, error) {
	ask := fmt.Sprintf("**Quick switch request**\nAn agent asked to switch the project %q to the Quick workflow (run cap %s to %s), which moves %d of its issues to another status.\nApprove it with your passkey in the web app, or reject it.",
		project.Title, workflowSetCapLabel(params.Before), workflowSetCapLabel(params.After), movedIssueCount(params))
	return h.openOwnerCard(ctx, host, ownerCard{actionType: decisionaction.ProjectWorkflowQuickSet, name: "project_workflow_quick_set", ask: ask, params: params})
}

func movedIssueCount(params decisionaction.ProjectWorkflowQuickParams) int64 {
	var n int64
	for _, required := range params.Required {
		n += required.IssueCount
	}
	return n
}

// executeProjectWorkflowQuickSetAction switches the project and moves its
// issues under the catalog (shared) and project locks the approval holds.
func executeProjectWorkflowQuickSetAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.ProjectWorkflowQuickParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	projectID, err := parseUUIDString(params.ProjectID)
	if err != nil {
		return nil, err
	}
	var targetID pgtype.UUID
	if params.WorkflowID != nil {
		if targetID, err = parseUUIDString(*params.WorkflowID); err != nil {
			return nil, err
		}
	}
	queries := execution.Queries
	workspaceID := execution.Action.WorkspaceID
	project, err := queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, staleBase("the project no longer exists")
	}
	if err != nil {
		return nil, err
	}
	target, normalized, err := loadSwitchTarget(ctx, queries, workspaceID, targetID)
	if errors.Is(err, errSwitchTargetMissing) {
		return nil, staleBase("the target workflow no longer exists in this workspace")
	}
	if err != nil {
		return nil, err
	}
	sw, err := planProjectSwitch(ctx, queries, project, target)
	if err != nil {
		return nil, err
	}
	current, err := projectWorkflowQuickParams(ctx, queries, sw, target, normalized, params.StatusMapping)
	if err != nil {
		return nil, err
	}
	if !sameOptionalID(current.WorkflowID, params.WorkflowID) || current.TotalIssues != params.TotalIssues ||
		current.Before != params.Before || current.After != params.After || !sameStatusCounts(current.Required, params.Required) {
		return nil, staleBase("the project, its issues or the run budget changed since the request; ask again")
	}
	if msg := checkMapping(sw.plan, params.StatusMapping, sw.accepts); msg != "" {
		return nil, staleBase("%s", msg)
	}
	updated, changed, cancelled, err := applyProjectSwitch(ctx, queries, sw, normalized, params.StatusMapping, uuidToString(execution.Approver.UserID))
	if err != nil {
		return nil, err
	}
	if err := insertAuditRow(ctx, queries, auditActor{Type: "member", ID: execution.Approver.UserID, Source: "decision_action"}, projectSwitchAudit(project, updated, len(changed))); err != nil {
		return nil, err
	}
	issueIDs := make([]string, 0, len(changed))
	for _, issue := range changed {
		issueIDs = append(issueIDs, uuidToString(issue.ID))
	}
	taskIDs := make([]string, 0, len(cancelled))
	for _, task := range cancelled {
		taskIDs = append(taskIDs, uuidToString(task.ID))
	}
	return json.Marshal(map[string]any{
		"project_id": params.ProjectID, "workflow_id": params.WorkflowID,
		"issues_remapped": len(changed), "issue_ids": issueIDs, "cancelled_task_ids": taskIDs,
	})
}

func sameStatusCounts(a, b []decisionaction.StatusCount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// publishProjectWorkflowQuickSet sends what an approved switch changed: the
// project, the issues it moved and the wakeups it ended.
func (h *Handler) publishProjectWorkflowQuickSet(ctx context.Context, action db.DecisionAction) error {
	var result struct {
		ProjectID        string   `json:"project_id"`
		IssueIDs         []string `json:"issue_ids"`
		CancelledTaskIDs []string `json:"cancelled_task_ids"`
	}
	if err := json.Unmarshal(action.Result, &result); err != nil {
		return err
	}
	projectID, err := parseUUIDString(result.ProjectID)
	if err != nil {
		return err
	}
	project, err := h.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: action.WorkspaceID})
	if err != nil {
		return err
	}
	approver := uuidToString(action.ApprovedByUserID)
	var issues []db.Issue
	for _, raw := range result.IssueIDs {
		id, err := parseUUIDString(raw)
		if err != nil {
			return err
		}
		issue, err := h.Queries.GetIssue(ctx, id)
		if err != nil {
			return err
		}
		issues = append(issues, issue)
	}
	var cancelled []db.AgentTaskQueue
	for _, raw := range result.CancelledTaskIDs {
		id, err := parseUUIDString(raw)
		if err != nil {
			return err
		}
		task, err := h.Queries.GetAgentTask(ctx, id)
		if err != nil {
			return err
		}
		cancelled = append(cancelled, task)
	}
	h.publishIssueSnapshots(ctx, issues, "member", approver)
	h.broadcastCancelledWakeups(ctx, action.WorkspaceID, cancelled)
	resp := projectToResponse(project)
	resp.IssueCount, resp.DoneCount = h.loadProjectIssueStats(ctx, project.WorkspaceID, project.ID)
	resp.ResourceCount = h.loadProjectResourceCount(ctx, project.ID)
	h.publish(protocol.EventProjectUpdated, uuidToString(action.WorkspaceID), "member", approver, map[string]any{"project": resp})
	return nil
}

// projectWorkflowQuickSetPreview shows the switch the card asks for: the
// project, its workflow and run cap before and after, and how many issues move.
func (h *Handler) projectWorkflowQuickSetPreview(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	var params decisionaction.ProjectWorkflowQuickParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	projectID, err := parseUUIDString(params.ProjectID)
	if err != nil {
		return nil, err
	}
	project, err := h.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: action.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // the executor ends the action with a failure
	}
	if err != nil {
		return nil, err
	}
	from, ok, err := h.workflowSetName(ctx, action.WorkspaceID, params.Before)
	if err != nil || !ok {
		return nil, err
	}
	to, ok, err := h.workflowSetName(ctx, action.WorkspaceID, params.After)
	if err != nil || !ok {
		return nil, err
	}
	return []ActionPreviewField{
		{"Project", project.Title, project.Title},
		{"Workflow", from, to},
		{"Run cap", workflowSetCapLabel(params.Before), workflowSetCapLabel(params.After)},
		{"Issues moved to another status", "0", strconv.FormatInt(movedIssueCount(params), 10)},
	}, nil
}
