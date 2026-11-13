package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const issueProjectSetType = "issue.project.set"

// issueProjectMove is the move an issue.project.set action asks for, with the
// workflow, status and run budget the issue has before and after it.
type issueProjectMove struct {
	target                   db.Project
	fromWorkflow, toWorkflow *issueworkflow.Definition
	before, after            budgetBinding
	fromStatus, toStatus     string
}

func (move issueProjectMove) widens() bool { return budgetWidens(move.before, move.after) }

// loadIssueProjectMove reads the target project and prices the move the way
// the non-interactive project guard does.
func loadIssueProjectMove(ctx context.Context, queries *db.Queries, issue db.Issue, params json.RawMessage) (issueProjectMove, error) {
	var wanted decisionaction.IssueProjectSetParams
	if err := json.Unmarshal(params, &wanted); err != nil {
		return issueProjectMove{}, err
	}
	projectID, err := parseUUIDString(wanted.ProjectID)
	if err != nil {
		return issueProjectMove{}, decisionIntentRefusal{http.StatusBadRequest, "invalid_project: params.project_id is not a project id"}
	}
	target, err := queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return issueProjectMove{}, decisionIntentRefusal{http.StatusBadRequest, "invalid_project: params.project_id is not a project in this workspace"}
	}
	if err != nil {
		return issueProjectMove{}, err
	}
	move := issueProjectMove{target: target}
	if move.fromWorkflow, err = followedWorkflow(ctx, queries, issue, issue.ProjectID); err != nil {
		return issueProjectMove{}, err
	}
	if move.toWorkflow, err = followedWorkflow(ctx, queries, issue, target.ID); err != nil {
		return issueProjectMove{}, err
	}
	move.before = workflowBinding(move.fromWorkflow, issue.MaxRunsOverride)
	move.after = workflowBinding(move.toWorkflow, issue.MaxRunsOverride)
	move.fromStatus, move.toStatus = issue.Status, issue.Status
	// A named workflow lists the statuses it allows: map one it lacks the way
	// an interactive move does. A move into a Default project keeps the status.
	if def := move.toWorkflow; def != nil && !def.IsDefault && !def.Has(issue.Status) {
		move.toStatus = issueworkflow.MoveTarget(*def, issue.Status, func(key string) string {
			return issuestatus.Category(ctx, queries, issue.WorkspaceID, key)
		})
	}
	if def := move.toWorkflow; def != nil && !def.IsDefault {
		if err := issueworkflow.CheckStatus(def, move.toStatus); err != nil {
			return issueProjectMove{}, decisionIntentRefusal{http.StatusBadRequest, "invalid_status: " + err.Error()}
		}
	}
	return move, nil
}

// followedWorkflow is the workflow the issue follows if it sits in projectID;
// nil when it follows none, or its override names a workflow that is gone.
func followedWorkflow(ctx context.Context, queries *db.Queries, issue db.Issue, projectID pgtype.UUID) (*issueworkflow.Definition, error) {
	if !issue.WorkflowID.Valid && !projectID.Valid {
		return nil, nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, queries, issue.WorkspaceID, issue.WorkflowID, projectID)
	if errors.Is(err, issueworkflow.ErrOverrideMissing) {
		return nil, nil
	}
	return def, err
}

// issueProjectPreconditions records the project the issue must still be in and
// whether the move widens its run budget, so approval refuses a card whose
// class no longer fits.
func issueProjectPreconditions(ctx context.Context, queries *db.Queries, issue db.Issue, draft *decisionActionDraft) (map[string]any, error) {
	if err := checkIssueTarget(draft.Params, issue.ID, http.StatusBadRequest); err != nil {
		return nil, err
	}
	move, err := loadIssueProjectMove(ctx, queries, issue, draft.Params)
	if err != nil {
		return nil, err
	}
	return map[string]any{"issue_id": uuidToString(issue.ID), "from_project_id": projectIDString(issue.ProjectID), "widens": move.widens()}, nil
}

// projectIDString is the id, or "" for an issue with no project.
func projectIDString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuidToString(id)
}

// requestClassFor is the escalation class of a proposed action: the registry
// minimum, raised to "spend" when an issue.project.set move widens the issue's
// run budget.
func (handler *Handler) requestClassFor(ctx context.Context, issue db.Issue, agentClass string, draft *decisionActionDraft) (string, error) {
	class := actionRequestClass(agentClass, draft.Definition.MinClass)
	if draft.Type != issueProjectSetType || class != "reversible" {
		return class, nil
	}
	move, err := loadIssueProjectMove(ctx, handler.Queries, issue, draft.Params)
	if err != nil {
		return "", err
	}
	if move.widens() {
		return "spend", nil
	}
	return class, nil
}

// withIssueTarget fills params.issue_id from the request's issue when an
// issue.project.set proposal leaves it out, as the CLI does. An explicit
// issue_id is kept, so a mismatch is still refused.
func withIssueTarget(wire *decisionActionWire, issueID pgtype.UUID) error {
	if wire == nil || wire.Type != issueProjectSetType {
		return nil
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(wire.Params, &params); err != nil || params == nil {
		return nil // draftDecisionAction refuses the shape
	}
	if _, found := params["issue_id"]; found {
		return nil
	}
	id, err := json.Marshal(uuidToString(issueID))
	if err != nil {
		return err
	}
	params["issue_id"] = id
	wire.Params, err = json.Marshal(params)
	return err
}

// executeIssueProjectSetAction moves the issue under the issue lock the
// approval holds. The move and its activity row commit with the approval.
func executeIssueProjectSetAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.IssueProjectSetParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	var stored struct {
		FromProjectID string `json:"from_project_id"`
		Widens        bool   `json:"widens"`
	}
	if err := json.Unmarshal(execution.Action.Preconditions, &stored); err != nil {
		return nil, err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return nil, err
	}
	queries := execution.Queries
	issue, err := queries.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	if projectIDString(issue.ProjectID) != stored.FromProjectID {
		return nil, actionFailure{Code: "stale_project", Message: "the issue moved to another project since this card was asked; ask again"}
	}
	move, err := loadIssueProjectMove(ctx, queries, issue, execution.Action.Params)
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		if strings.HasPrefix(refusal.message, "invalid_status") {
			return nil, actionFailure{Code: "status_unavailable", Message: "the target workflow has no status this issue can move to"}
		}
		return nil, actionFailure{Code: "project_missing", Message: "the target project no longer exists in this workspace"}
	}
	if err != nil {
		return nil, err
	}
	if err := service.RequireActiveProject(ctx, queries, issue.WorkspaceID, move.target.ID); errors.Is(err, service.ErrProjectInactive) {
		return nil, actionFailure{Code: "project_inactive", Message: "the target project is not active"}
	} else if err != nil {
		return nil, err
	}
	// The card was asked as reversible; a target budget that grew since needs a new ask.
	if move.widens() && !stored.Widens {
		return nil, actionFailure{Code: "stale_budget", Message: "the move now widens the run budget; ask again"}
	}
	moved, err := queries.SetIssueProjectFromDecision(ctx, db.SetIssueProjectFromDecisionParams{
		ID: issue.ID, WorkspaceID: issue.WorkspaceID, ProjectID: move.target.ID, FromProjectID: issue.ProjectID, Status: move.toStatus,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, actionFailure{Code: "stale_project", Message: "the issue moved to another project while approving; ask again"}
	}
	if err != nil {
		return nil, err
	}
	details, err := json.Marshal(map[string]any{
		"from_project_id": stored.FromProjectID, "to_project_id": uuidToString(moved.ProjectID),
		"action_id": uuidToString(execution.Action.ID), "widens_run_budget": move.widens(),
		"from_status": move.fromStatus, "to_status": moved.Status,
	})
	if err != nil {
		return nil, err
	}
	if _, err := queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "member", Valid: true},
		ActorID: execution.Approver.UserID, Action: "project_changed", Details: details,
	}); err != nil {
		return nil, err
	}
	result := map[string]any{
		"project_id": uuidToString(moved.ProjectID), "from_project_id": stored.FromProjectID,
		"from_status": move.fromStatus, "to_status": moved.Status,
	}
	if moved.Status != move.fromStatus {
		trigger := service.BackgroundStatusTrigger(service.StatusCauseDecision,
			service.StatusCauseActor{Type: "member", ID: uuidToString(execution.Approver.UserID)},
			map[string]string{"action_id": uuidToString(execution.Action.ID)})
		row, err := service.RecordStatusChange(ctx, queries, moved, move.fromStatus, moved.Status, trigger, nil)
		if err != nil {
			return nil, err
		}
		result["status_activity_id"] = uuidToString(row.ID)
	}
	if move.toWorkflow != nil {
		result["workflow_id"] = uuidToString(move.toWorkflow.ID)
	}
	return json.Marshal(result)
}

// issueProjectSetPreview shows the project, workflow and run budget the issue
// has now and would have after the move.
func (handler *Handler) issueProjectSetPreview(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	var params decisionaction.IssueProjectSetParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return nil, err
	}
	issue, err := handler.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	move, err := loadIssueProjectMove(ctx, handler.Queries, issue, action.Params)
	if errors.As(err, new(decisionIntentRefusal)) {
		return nil, nil // the executor ends the action with a failure
	}
	if err != nil {
		return nil, err
	}
	snapshot, err := service.LoadRunBudgetSnapshot(ctx, handler.Queries, issue)
	if err != nil {
		return nil, err
	}
	fromName := "No project"
	if issue.ProjectID.Valid {
		from, err := handler.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return nil, err
		}
		fromName = from.Title
	}
	used := strconv.FormatInt(snapshot.RunsUsed, 10)
	verdict := "This move does not widen the run budget"
	if move.widens() {
		verdict = "This move widens the run budget"
	}
	return []ActionPreviewField{
		{"Project", fromName, move.target.Title},
		{"Status", move.fromStatus, move.toStatus},
		{"Workflow", workflowLabel(move.fromWorkflow), workflowLabel(move.toWorkflow)},
		{"Run cap", capLabel(move.before), capLabel(move.after)},
		{"Runs used", used, used},
		{"Run budget", "", verdict},
	}, nil
}

func workflowLabel(def *issueworkflow.Definition) string {
	if def == nil {
		return "None"
	}
	return def.Name
}

func capLabel(binding budgetBinding) string {
	if !binding.budgeted {
		return "No cap"
	}
	return strconv.Itoa(binding.cap)
}

// publishIssueProjectSet tells open clients the issue moved, with the same
// issue:updated payload an interactive move sends, so project lists refetch.
func (handler *Handler) publishIssueProjectSet(ctx context.Context, action db.DecisionAction) error {
	var params decisionaction.IssueProjectSetParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return err
	}
	var moved struct {
		FromProjectID string `json:"from_project_id"`
		FromStatus    string `json:"from_status"`
		StatusRowID   string `json:"status_activity_id"`
	}
	if err := json.Unmarshal(action.Result, &moved); err != nil {
		return err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return err
	}
	issue, err := handler.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return err
	}
	resp := issueToResponse(issue, handler.getIssuePrefix(ctx, issue.WorkspaceID))
	handler.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	actorID := uuidToString(action.ApprovedByUserID)
	payload := map[string]any{
		"issue":           resp,
		"project_changed": projectIDString(issue.ProjectID) != moved.FromProjectID,
		"status_changed":  moved.StatusRowID != "",
		"prev_status":     moved.FromStatus,
		"action_id":       uuidToString(action.ID),
	}
	var rec *statusRecord
	if moved.StatusRowID != "" {
		rowID, err := parseUUIDString(moved.StatusRowID)
		if err != nil {
			return err
		}
		row, err := handler.Queries.GetActivity(ctx, rowID)
		if err != nil {
			return err
		}
		rec = &statusRecord{activity: &row}
		payload["activity_id"] = moved.StatusRowID
	}
	handler.publish(protocol.EventIssueUpdated, uuidToString(issue.WorkspaceID), "member", actorID, payload)
	handler.publishRecordedStatusActivity(rec, "member", actorID)
	return nil
}
