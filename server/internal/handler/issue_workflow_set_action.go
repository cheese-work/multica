package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// issue.workflow.set@1 (CHE-5505): an agent's `issue update --workflow` that
// widens the issue's run budget opens one owner card instead of failing. The
// card seals the budget binding before and after; the executor recomputes both
// under the locks and ends the action as stale_base when any field differs.

const (
	issueWorkflowSetType = "issue.workflow.set"
	staleBaseCode        = "stale_base"
)

// sealWorkflowBinding is the binding of an issue that follows explicit, or its
// project's workflow when explicit is unset. def is nil when the override names
// no workflow in the workspace, which the run guard reads as unbudgeted.
func sealWorkflowBinding(ctx context.Context, q issueworkflow.Querier, workspaceID, explicit, projectID pgtype.UUID, override pgtype.Int4) (decisionaction.IssueWorkflowSetBinding, budgetBinding, *issueworkflow.Definition, error) {
	def, project, err := issueworkflow.ForIssue(ctx, q, workspaceID, explicit, projectID)
	if err != nil && !errors.Is(err, issueworkflow.ErrOverrideMissing) {
		return decisionaction.IssueWorkflowSetBinding{}, budgetBinding{}, nil, err
	}
	if errors.Is(err, issueworkflow.ErrOverrideMissing) {
		def = nil
	}
	sealed := decisionaction.IssueWorkflowSetBinding{Source: decisionaction.WorkflowSourceNone}
	switch {
	case explicit.Valid:
		sealed.Source = decisionaction.WorkflowSourceIssue
	case projectID.Valid:
		sealed.Source = decisionaction.WorkflowSourceProject
	}
	if project.ID.Valid {
		sealed.ProjectID = uuidToString(project.ID)
		if project.WorkflowID.Valid {
			sealed.ProjectWorkflowID = uuidToString(project.WorkflowID)
		}
	}
	if def != nil && def.ID.Valid {
		sealed.WorkflowID = uuidToString(def.ID)
	}
	if def == nil && explicit.Valid {
		sealed.WorkflowID = uuidToString(explicit)
	}
	binding := workflowBinding(def, override)
	if binding.budgeted {
		sealed.Budgeted, sealed.Cap = true, binding.cap
		sealed.Start = binding.start.UTC().Format(time.RFC3339Nano)
	}
	return sealed, binding, def, nil
}

func uuidStringPtr(id pgtype.UUID) *string {
	if !id.Valid {
		return nil
	}
	s := uuidToString(id)
	return &s
}

// issueWorkflowSetParams seals the move from the issue's current binding to
// the one following target.
func issueWorkflowSetParams(ctx context.Context, q issueworkflow.Querier, issue db.Issue, target pgtype.UUID) (decisionaction.IssueWorkflowSetParams, error) {
	before, _, _, err := sealWorkflowBinding(ctx, q, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID, issue.MaxRunsOverride)
	if err != nil {
		return decisionaction.IssueWorkflowSetParams{}, err
	}
	after, _, _, err := sealWorkflowBinding(ctx, q, issue.WorkspaceID, target, issue.ProjectID, issue.MaxRunsOverride)
	if err != nil {
		return decisionaction.IssueWorkflowSetParams{}, err
	}
	return decisionaction.IssueWorkflowSetParams{
		IssueID: uuidToString(issue.ID), WorkflowID: uuidStringPtr(target), ExplicitWorkflowID: uuidStringPtr(issue.WorkflowID),
		Before: before, After: after,
	}, nil
}

func workflowSetAsk(issue db.Issue, params decisionaction.IssueWorkflowSetParams) string {
	return fmt.Sprintf("**Run budget request**\nAn agent asked to move [Issue](mention://issue/%s) to a workflow that allows more runs (cap %d to %d).\nApprove it with your passkey in the web app, or reject it.", uuidToString(issue.ID), params.Before.Cap, params.After.Cap)
}

// openIssueWorkflowSetCard opens the card for every workspace owner, and
// returns the card an owner already has open for the same sealed move. It runs
// in its own transaction: opening a decision takes the workspace and topic
// locks, which come before the issue lock.
func (h *Handler) openIssueWorkflowSetCard(ctx context.Context, issue db.Issue, params decisionaction.IssueWorkflowSetParams) (RunBudgetActionResponse, error) {
	var out RunBudgetActionResponse
	var results []decisionRequestResult
	raw, err := json.Marshal(params)
	if err != nil {
		return out, err
	}
	err = h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		owners, err := q.ListWorkspaceOwnerUserIDs(ctx, issue.WorkspaceID)
		if err != nil {
			return err
		}
		plan, err := h.planRunBudgetDecisions(ctx, q, issue, owners)
		if err != nil {
			return err
		}
		if len(plan.lanes) == 0 {
			return decisionIntentRefusal{http.StatusConflict, "no owner can be asked to approve"}
		}
		workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
		if err != nil {
			return err
		}
		draft, err := draftDecisionAction(&decisionActionWire{Type: issueWorkflowSetType, Version: 1, Params: raw})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(draft.Params)
		input := decisionRequestInput{
			Ask: workflowSetAsk(issue, params), EscalationClass: actionRequestClass("reversible", draft.Definition.MinClass), Action: draft,
			Fingerprint: serverDecisionFingerprint + "issue_workflow_set:" + hex.EncodeToString(digest[:]),
		}
		results = nil
		out = RunBudgetActionResponse{}
		for _, lane := range plan.lanes {
			link := func(request db.DecisionRequest, action db.DecisionAction) {
				out.Requests = append(out.Requests, RunBudgetActionLink{
					RequestID: uuidToString(request.ID), ActionID: uuidToString(action.ID),
					ApproveURL: "/" + workspace.Slug + "/issues/" + uuidToString(issue.ID) + "?approve=" + uuidToString(action.ID),
				})
			}
			open, err := q.FindOpenServerDecisionRequest(ctx, db.FindOpenServerDecisionRequestParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, HumanUserID: lane.human, Fingerprint: input.Fingerprint})
			if err == nil {
				action, err := q.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: open.ID, WorkspaceID: issue.WorkspaceID})
				if err != nil {
					return err
				}
				link(open, action)
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			input.HumanUserID = lane.human
			result, err := h.openRunBudgetDecision(ctx, q, issue, plan, lane, input)
			if err != nil {
				return err
			}
			results = append(results, result)
			link(result.Request, *result.Action)
		}
		return nil
	})
	if err != nil {
		return RunBudgetActionResponse{}, err
	}
	h.publishRunBudgetDecisions(issue, results)
	out.RunBudgetActionLink = out.Requests[0]
	return out, nil
}

func sameOptionalID(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func staleBase(format string, args ...any) error {
	return actionFailure{Code: staleBaseCode, Message: fmt.Sprintf(format, args...)}
}

// executeIssueWorkflowSetAction applies the approved move. The approval holds,
// in order: the issue's budget boundary, the status catalog (shared), the
// project the issue was in when the plan read it, and the issue row. It reads
// no workflow row under a lock; the catalog lock keeps the workflow's cap and
// budget start still. Anything that differs from the sealed move is stale_base.
func executeIssueWorkflowSetAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.IssueWorkflowSetParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return nil, err
	}
	var target pgtype.UUID
	if params.WorkflowID != nil {
		if target, err = parseUUIDString(*params.WorkflowID); err != nil {
			return nil, err
		}
	}
	queries := execution.Queries
	issue, err := queries.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issueID, WorkspaceID: execution.Action.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, staleBase("the issue no longer exists")
	}
	if err != nil {
		return nil, err
	}
	// The plan locked the project the issue was in when it read it. A different
	// project now has no lock here, so the move is not applied.
	if issue.ProjectID != execution.ProjectID {
		return nil, staleBase("the issue moved to another project since the request; ask again")
	}
	if target.Valid {
		if _, err := queries.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: target, WorkspaceID: issue.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
			return nil, staleBase("the target workflow no longer exists in this workspace")
		} else if err != nil {
			return nil, err
		}
	}
	current, err := issueWorkflowSetParams(ctx, queries, issue, target)
	if err != nil {
		return nil, err
	}
	if !sameOptionalID(current.ExplicitWorkflowID, params.ExplicitWorkflowID) {
		return nil, staleBase("the issue's own workflow changed since the request; ask again")
	}
	if current.Before != params.Before || current.After != params.After {
		return nil, staleBase("the run budget of the issue or the target workflow changed since the request; ask again")
	}
	next, _, err := issueworkflow.ForIssue(ctx, queries, issue.WorkspaceID, target, issue.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := issueworkflow.CheckStatus(next, issue.Status); err != nil {
		return nil, actionFailure{Code: "status_not_in_workflow", Message: err.Error()}
	}
	live, err := queries.HasLiveTaskForIssue(ctx, issue.ID)
	if err != nil {
		return nil, err
	}
	if live {
		return nil, actionFailure{Code: "issue_has_live_task", Message: "the issue has a task that can still run; wait for it to finish or cancel it, then ask again"}
	}
	if _, err := queries.SetIssueWorkflow(ctx, db.SetIssueWorkflowParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, WorkflowID: target}); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"issue_id": params.IssueID, "workflow_id": params.WorkflowID})
}

// publishIssueWorkflowSet sends the issue snapshot of an approved move.
func (h *Handler) publishIssueWorkflowSet(ctx context.Context, action db.DecisionAction) error {
	var params struct {
		IssueID string `json:"issue_id"`
	}
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return err
	}
	issue, err := h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return err
	}
	h.publishIssueSnapshots(ctx, []db.Issue{issue}, "member", uuidToString(action.ApprovedByUserID))
	return nil
}

// issueWorkflowSetPreview renders the sealed move for the card: the issue, the
// workflow and run cap before and after, and the delta. It reads the names the
// owner sees; the binding itself is checked by the executor at approve.
func (h *Handler) issueWorkflowSetPreview(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	var params decisionaction.IssueWorkflowSetParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return nil, err
	}
	issue, err := h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	from, ok, err := h.workflowSetName(ctx, issue.WorkspaceID, params.Before)
	if err != nil || !ok {
		return nil, err
	}
	to, ok, err := h.workflowSetName(ctx, issue.WorkspaceID, params.After)
	if err != nil || !ok {
		return nil, err
	}
	label := fmt.Sprintf("%s-%d %s", h.getIssuePrefix(ctx, issue.WorkspaceID), issue.Number, issue.Title)
	verdict := "This move does not widen the run budget"
	if params.After.Budgeted && (!params.Before.Budgeted || params.After.Cap > params.Before.Cap) {
		verdict = "This move widens the run budget"
		if params.Before.Budgeted {
			verdict = fmt.Sprintf("This move widens by %d runs", params.After.Cap-params.Before.Cap)
		}
	}
	return []ActionPreviewField{
		{"Issue", label, label},
		{"Workflow", from, to},
		{"Run cap", workflowSetCapLabel(params.Before), workflowSetCapLabel(params.After)},
		{"Run budget", "", verdict},
	}, nil
}

func workflowSetCapLabel(b decisionaction.IssueWorkflowSetBinding) string {
	if !b.Budgeted {
		return "No cap"
	}
	return strconv.Itoa(b.Cap)
}

// workflowSetName is the workflow's name, "None" when the binding has none.
// ok is false when the named workflow is gone: the executor ends the action
// with a failure, so the card shows no preview.
func (h *Handler) workflowSetName(ctx context.Context, workspaceID pgtype.UUID, b decisionaction.IssueWorkflowSetBinding) (string, bool, error) {
	if b.WorkflowID == "" {
		return "None", true, nil
	}
	id, err := parseUUIDString(b.WorkflowID)
	if err != nil {
		return "", false, err
	}
	row, err := h.Queries.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: id, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.Name, true, nil
}
