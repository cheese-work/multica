package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunBudgetActionRequest asks for an issue's run budget to be raised or topped
// up. NewCap belongs to a raise only; WindowID is the window the caller saw.
type RunBudgetActionRequest struct {
	Kind     string `json:"kind"`
	NewCap   int    `json:"new_cap"`
	WindowID string `json:"window_id"`
}

// RunBudgetActionLink is one opened request. Approving it needs an owner and a
// passkey; the caller's own credential never does.
type RunBudgetActionLink struct {
	RequestID  string `json:"request_id"`
	ActionID   string `json:"action_id"`
	ApproveURL string `json:"approve_url"`
}

// RunBudgetActionResponse is the first link at top level, and every link in
// Requests: one per owner asked.
type RunBudgetActionResponse struct {
	RunBudgetActionLink
	Requests []RunBudgetActionLink `json:"requests"`
}

var runBudgetActionTypes = map[string]string{"raise": "issue.run_budget.raise", "top_up": "issue.run_budget.top_up"}

// RequestIssueRunBudgetAction opens a server-origin spend request with a raise
// or top-up action attached (CHE-2855). Any human credential may call it; it
// only asks. The same kind, window and cap return the request already open.
func (h *Handler) RequestIssueRunBudgetAction(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req RunBudgetActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	_, known := runBudgetActionTypes[req.Kind]
	if !known {
		writeError(w, http.StatusBadRequest, "kind must be raise or top_up")
		return
	}
	if (req.Kind == "raise") != (req.NewCap != 0) {
		writeError(w, http.StatusBadRequest, "new_cap is for a raise only, and a raise needs it")
		return
	}
	var results []decisionRequestResult
	var links []RunBudgetActionLink
	err := h.withPgxTx(r.Context(), func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		recipients, err := h.runBudgetActionRecipients(r, q, issue, userID)
		if err != nil {
			return err
		}
		plan, err := h.planRunBudgetDecisions(r.Context(), q, issue, recipients)
		if err != nil {
			return err
		}
		if len(plan.lanes) == 0 {
			return decisionIntentRefusal{http.StatusConflict, "no owner can be asked to approve"}
		}
		links, results, err = h.openRunBudgetActionCards(r.Context(), q, issue, plan, req.Kind, req.NewCap, req.WindowID, runBudgetActionAsk(issue, req))
		if err != nil || len(results) == 0 {
			return err
		}
		return recordRunBudgetAsk(r.Context(), q, issue, "member", parseUUID(userID), runBudgetActionTypes[req.Kind], req.WindowID)
	})
	if err != nil {
		writeDecisionError(w, err, "run budget request")
		return
	}
	created := len(results) > 0
	h.publishRunBudgetDecisions(issue, results)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, RunBudgetActionResponse{RunBudgetActionLink: links[0], Requests: links})
}

// runBudgetActionRecipients is the caller when the caller owns the workspace,
// else every owner: a request goes only to someone who can approve it.
func (h *Handler) runBudgetActionRecipients(r *http.Request, q *db.Queries, issue db.Issue, userID string) ([]pgtype.UUID, error) {
	caller, err := q.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: parseUUID(userID), WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return nil, err
	}
	if caller.Role == "owner" {
		return []pgtype.UUID{caller.UserID}, nil
	}
	return q.ListWorkspaceOwnerUserIDs(r.Context(), issue.WorkspaceID)
}

func runBudgetActionAsk(issue db.Issue, req RunBudgetActionRequest) string {
	what := "open a new run window"
	if req.Kind == "raise" {
		what = fmt.Sprintf("raise the run cap to %d", req.NewCap)
	}
	return fmt.Sprintf("**Run budget request**\nA person asked to %s on [Issue](mention://issue/%s).\nApprove it with your passkey in the web app, or reject it.", what, uuidToString(issue.ID))
}

// openRunBudgetActionCards opens the raise or top-up action card of one window
// for every planned lane, and returns the card a lane already has open without
// a second one: the fingerprint names kind, window and cap, so a repeat request
// and a repeat refusal in one window share a card. Both entry points use it
// (CHE-5303).
func (h *Handler) openRunBudgetActionCards(ctx context.Context, q *db.Queries, issue db.Issue, plan runBudgetDecisionPlan, kind string, newCap int, windowID, ask string) ([]RunBudgetActionLink, []decisionRequestResult, error) {
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	params := map[string]any{"issue_id": uuidToString(issue.ID), "window_id": windowID}
	if kind == "raise" {
		params["new_cap"] = newCap
	}
	if refusal, err := service.FindOpenRunBudgetRefusal(ctx, q, issue.ID, windowID); err == nil {
		params["refusal_id"] = uuidToString(refusal)
	} else if !errors.Is(err, service.ErrRunBudgetRefusalNotFound) {
		return nil, nil, err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, nil, err
	}
	draft, err := draftDecisionAction(&decisionActionWire{Type: runBudgetActionTypes[kind], Version: 1, Params: raw})
	if err != nil {
		return nil, nil, err
	}
	input := decisionRequestInput{
		Ask: ask, EscalationClass: actionRequestClass("reversible", draft.Definition.MinClass), Action: draft,
		Fingerprint: fmt.Sprintf("%srun_budget_action:%s:%s:%d", serverDecisionFingerprint, kind, windowID, newCap),
	}
	var links []RunBudgetActionLink
	var results []decisionRequestResult
	for _, lane := range plan.lanes {
		link := func(request db.DecisionRequest, action db.DecisionAction) {
			links = append(links, RunBudgetActionLink{
				RequestID: uuidToString(request.ID), ActionID: uuidToString(action.ID),
				ApproveURL: "/" + workspace.Slug + "/issues/" + uuidToString(issue.ID) + "?approve=" + uuidToString(action.ID),
			})
		}
		open, err := q.FindOpenServerDecisionRequest(ctx, db.FindOpenServerDecisionRequestParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, HumanUserID: lane.human, Fingerprint: input.Fingerprint})
		if err == nil {
			action, err := q.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: open.ID, WorkspaceID: issue.WorkspaceID})
			if err != nil {
				return nil, nil, err
			}
			link(open, action)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		input.HumanUserID = lane.human
		result, err := h.openRunBudgetDecision(ctx, q, issue, plan, lane, input)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, result)
		link(result.Request, *result.Action)
	}
	return links, results, nil
}

// runBudgetCardRecipients is who a refusal's top-up card goes to: the requester
// when they own the workspace, else every owner. Approving needs an owner and
// a passkey, so a card for anyone else could never be approved.
func runBudgetCardRecipients(ctx context.Context, q *db.Queries, issue db.Issue, recipients []pgtype.UUID) ([]pgtype.UUID, error) {
	for _, userID := range recipients {
		member, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: issue.WorkspaceID})
		if err == nil && member.Role == "owner" {
			return []pgtype.UUID{userID}, nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	return q.ListWorkspaceOwnerUserIDs(ctx, issue.WorkspaceID)
}

// runBudgetRefusalAsk is the card's text when the platform itself refused a run.
func runBudgetRefusalAsk(issue db.Issue) string {
	return fmt.Sprintf("**Run budget used up**\nThe platform refused the next run on [Issue](mention://issue/%s). Nothing runs on it until a new run window opens.\nApprove a top-up with your passkey in the web app to open one, or decline to keep it stopped.", uuidToString(issue.ID))
}
