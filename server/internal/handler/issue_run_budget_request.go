package handler

import (
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
	actionType, known := runBudgetActionTypes[req.Kind]
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
	created := false
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
		workspace, err := q.GetWorkspace(r.Context(), issue.WorkspaceID)
		if err != nil {
			return err
		}
		params := map[string]any{"issue_id": uuidToString(issue.ID), "window_id": req.WindowID}
		if req.Kind == "raise" {
			params["new_cap"] = req.NewCap
		}
		if refusal, err := service.FindOpenRunBudgetRefusal(r.Context(), q, issue.ID, req.WindowID); err == nil {
			params["refusal_id"] = uuidToString(refusal)
		} else if !errors.Is(err, service.ErrRunBudgetRefusalNotFound) {
			return err
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		draft, err := draftDecisionAction(&decisionActionWire{Type: actionType, Version: 1, Params: raw})
		if err != nil {
			return err
		}
		input := decisionRequestInput{
			Ask: runBudgetActionAsk(issue, req), EscalationClass: actionRequestClass("reversible", draft.Definition.MinClass), Action: draft,
			Fingerprint: fmt.Sprintf("%srun_budget_action:%s:%s:%d", serverDecisionFingerprint, req.Kind, req.WindowID, req.NewCap),
		}
		for _, lane := range plan.lanes {
			link := func(request db.DecisionRequest, action db.DecisionAction) {
				links = append(links, RunBudgetActionLink{
					RequestID: uuidToString(request.ID), ActionID: uuidToString(action.ID),
					ApproveURL: "/" + workspace.Slug + "/issues/" + uuidToString(issue.ID) + "?approve=" + uuidToString(action.ID),
				})
			}
			open, err := q.FindOpenServerDecisionRequest(r.Context(), db.FindOpenServerDecisionRequestParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, HumanUserID: lane.human, Fingerprint: input.Fingerprint})
			if err == nil {
				action, err := q.GetDecisionActionByRequest(r.Context(), db.GetDecisionActionByRequestParams{DecisionRequestID: open.ID, WorkspaceID: issue.WorkspaceID})
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
			result, err := h.openRunBudgetDecision(r.Context(), q, issue, plan, lane, input)
			if err != nil {
				return err
			}
			results = append(results, result)
			link(result.Request, *result.Action)
		}
		created = len(results) > 0
		return nil
	})
	if err != nil {
		writeDecisionError(w, err, "run budget request")
		return
	}
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
