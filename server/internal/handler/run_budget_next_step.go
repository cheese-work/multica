package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrorCodeRunBudgetExhausted is the typed error code of a run the budget refused.
const ErrorCodeRunBudgetExhausted = "run_budget_exhausted"

// runBudgetAskAction is the activity row written once per opened run-budget
// ask: the origin source of CHE-5236's metric M2.
const runBudgetAskAction = "run_budget_ask"

// runBudgetCard is the raise or top-up card open for an issue's current window.
type runBudgetCard struct {
	RequestID string `json:"request_id"`
	ActionID  string `json:"action_id"`
	URL       string `json:"url"`
}

// runBudgetNextStep is what a reader of a spent or near budget does next
// (CHE-5503): the open card to wait for, or the one command that asks for more.
type runBudgetNextStep struct {
	Identifier   string
	WorkflowID   string
	WorkflowName string
	OpenCard     *runBudgetCard
	NextCommand  string
}

func isRunBudgetActionType(actionType string) bool {
	return actionType == runBudgetRaiseType || actionType == runBudgetTopUpType
}

// loadRunBudgetNextStep reads the issue's open card for the snapshot's window
// and words the next command. An issue without a budget has no next step.
func (h *Handler) loadRunBudgetNextStep(ctx context.Context, q *db.Queries, issue db.Issue, snap service.RunBudgetSnapshot) (runBudgetNextStep, error) {
	if !snap.Budgeted {
		return runBudgetNextStep{}, nil
	}
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return runBudgetNextStep{}, err
	}
	next := runBudgetNextStep{Identifier: fmt.Sprintf("%s-%d", issuePrefixForWorkspace(workspace), issue.Number)}
	def, _, err := issueworkflow.ForIssue(ctx, q, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil {
		return runBudgetNextStep{}, err
	}
	if def != nil {
		next.WorkflowID, next.WorkflowName = uuidToString(def.ID), def.Name
	}
	card, err := q.FindOpenRunBudgetCard(ctx, db.FindOpenRunBudgetCardParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, WindowID: snap.WindowID})
	switch {
	case err == nil:
		next.OpenCard = &runBudgetCard{
			RequestID: uuidToString(card.RequestID), ActionID: uuidToString(card.ActionID),
			URL: "/" + workspace.Slug + "/issues/" + uuidToString(issue.ID) + "?approve=" + uuidToString(card.ActionID),
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return runBudgetNextStep{}, err
	}
	next.NextCommand = runBudgetNextCommand(next.Identifier, snap, next.OpenCard)
	return next, nil
}

// runBudgetNextCommand is the one thing to do next. With a card open it says to
// wait for that card; a repeat ask would only return it.
func runBudgetNextCommand(identifier string, snap service.RunBudgetSnapshot, card *runBudgetCard) string {
	if card != nil {
		return fmt.Sprintf("Wait for the approval card %s. Do not open another.", card.URL)
	}
	if snap.RaisePreview == nil {
		return fmt.Sprintf("The run cap is at its maximum. A member opens a new run window with: multica issue budget top-up %s", identifier)
	}
	return fmt.Sprintf("multica issue budget raise %s --runs %d", identifier, snap.RaisePreview.LimitAfter)
}

// runBudgetExhaustedResponse is the refusal a run gets when the budget is spent.
type runBudgetExhaustedResponse struct {
	dispatchBlockedResponse
	Code        string         `json:"code"`
	RunsUsed    int64          `json:"runs_used"`
	RunsLimit   int            `json:"runs_limit"`
	WorkflowID  string         `json:"workflow_id"`
	WindowID    string         `json:"window_id"`
	OpenCard    *runBudgetCard `json:"open_card"`
	NextCommand string         `json:"next_command"`
}

// writeRunBudgetExhausted answers a refused run with its typed next step. A
// budget that cannot be read falls back to the bare reason code.
func (h *Handler) writeRunBudgetExhausted(w http.ResponseWriter, r *http.Request, issue db.Issue) {
	snap, err := service.LoadRunBudgetSnapshot(r.Context(), h.Queries, issue)
	var next runBudgetNextStep
	if err == nil {
		next, err = h.loadRunBudgetNextStep(r.Context(), h.Queries, issue, snap)
	}
	if err != nil {
		slog.Warn("run budget: next step failed", "err", err, "issue_id", uuidToString(issue.ID))
		h.writeDispatchBlocked(w, http.StatusForbidden, ReasonRunBudgetExhausted)
		return
	}
	writeJSON(w, http.StatusForbidden, runBudgetExhaustedResponse{
		dispatchBlockedResponse: dispatchBlockedResponse{Error: dispatchBlockedFallbackMessage(ReasonRunBudgetExhausted), ReasonCode: ReasonRunBudgetExhausted},
		Code:                    ErrorCodeRunBudgetExhausted,
		RunsUsed:                snap.RunsUsed, RunsLimit: snap.RunsLimit,
		WorkflowID: next.WorkflowID, WindowID: snap.WindowID,
		OpenCard: next.OpenCard, NextCommand: next.NextCommand,
	})
}

// runBudgetView is the budget read plus what to do next.
type runBudgetView struct {
	service.RunBudgetSnapshot
	OpenCard    *runBudgetCard `json:"open_card"`
	NextCommand string         `json:"next_command,omitempty"`
}

// recordRunBudgetAsk writes the one activity row of an opened ask. A system ask
// has no actor id, as the other system rows of the budget.
func recordRunBudgetAsk(ctx context.Context, q *db.Queries, issue db.Issue, actorType string, actorID pgtype.UUID, actionType, windowID string) error {
	details, err := json.Marshal(map[string]string{"action_type": actionType, "window_id": windowID})
	if err != nil {
		return err
	}
	_, err = q.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: actorType, Valid: true},
		ActorID: actorID, Action: runBudgetAskAction, Details: details,
	})
	return err
}

// runBudgetWindowOf is the window a raise or top-up action names.
func runBudgetWindowOf(params json.RawMessage) (string, error) {
	var p struct {
		WindowID string `json:"window_id"`
	}
	err := json.Unmarshal(params, &p)
	return p.WindowID, err
}

// openRunBudgetProposal is the card already open for the window a raise or
// top-up proposal names, so a repeat ask returns it instead of opening another.
func (h *Handler) openRunBudgetProposal(ctx context.Context, issue db.Issue, draft *decisionActionDraft) (db.DecisionRequest, bool, error) {
	if !isRunBudgetActionType(draft.Type) {
		return db.DecisionRequest{}, false, nil
	}
	window, err := runBudgetWindowOf(draft.Params)
	if err != nil {
		return db.DecisionRequest{}, false, err
	}
	card, err := h.Queries.FindOpenRunBudgetCard(ctx, db.FindOpenRunBudgetCardParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, WindowID: window})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.DecisionRequest{}, false, nil
	}
	if err != nil {
		return db.DecisionRequest{}, false, err
	}
	request, err := h.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: card.RequestID})
	return request, err == nil, err
}
