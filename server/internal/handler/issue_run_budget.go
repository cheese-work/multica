package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RaiseIssueRunBudgetRequest sets the issue's own run cap to Runs total.
type RaiseIssueRunBudgetRequest struct {
	Runs int `json:"runs"`
}

// GetIssueRunBudget returns the issue's run budget: runs used in the open
// window against the limit in force, its state, and the suggested raise. Any
// reader of the issue may see it; an issue with no budget says so.
func (h *Handler) GetIssueRunBudget(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	snap, err := service.LoadRunBudgetSnapshot(r.Context(), h.Queries, issue)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the run budget")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// RaiseIssueRunBudget lifts one issue's run cap (CHE-2226) so a person can pay
// for more runs on that issue without raising the cap of every issue on the
// workflow. Members only: the actor comes from the authenticated request, never
// from the body, and an agent cannot spend its own budget.
func (h *Handler) RaiseIssueRunBudget(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	actorType, actorID := h.resolveActor(r, userID, uuidToString(issue.WorkspaceID))
	if actorType != "member" {
		writeError(w, http.StatusForbidden, "only a person can raise an issue's run budget")
		return
	}
	var req RaiseIssueRunBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Runs > issueworkflow.MaxRunsPerIssueLimit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("runs must not exceed %d", issueworkflow.MaxRunsPerIssueLimit))
		return
	}
	def, _, err := issueworkflow.ForIssue(r.Context(), h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil {
		writeError(w, http.StatusBadRequest, "the issue has no workflow run budget to raise")
		return
	}
	workflowMax := def.EffectiveMaxRuns()
	current := workflowMax
	if issue.MaxRunsOverride.Valid && int(issue.MaxRunsOverride.Int32) > current {
		current = int(issue.MaxRunsOverride.Int32)
	}
	if req.Runs <= current {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("runs must be above the cap now in force (%d)", current))
		return
	}
	// The cap and the audit of who raised it commit together: a raise with no
	// record of the person behind it must not stay in force.
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to raise the run budget")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	// The window opens, and the cap is read, under the issue lock: a raise
	// that waited behind another one starts from the cap that one left.
	current, err = service.OpenRunBudgetWindow(r.Context(), qtx, issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to raise the run budget")
		return
	}
	if req.Runs <= current {
		writeError(w, http.StatusConflict, fmt.Sprintf("the cap changed while raising it; it is now %d", current))
		return
	}
	raised, err := qtx.RaiseIssueMaxRuns(r.Context(), db.RaiseIssueMaxRunsParams{
		ID: issue.ID, WorkspaceID: issue.WorkspaceID, Runs: int32(req.Runs), WorkflowMax: int32(workflowMax),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "the cap changed while raising it; read it again")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to raise the run budget")
		return
	}
	body, _ := json.Marshal(map[string]any{"from": current, "to": raised.Int32})
	activity, err := qtx.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: actorType, Valid: true},
		ActorID: parseUUID(actorID), Action: "run_budget_raised", Details: body,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the run budget raise")
		return
	}
	if err := service.RecordRunBudgetRaise(r.Context(), qtx, issue, activity.ID, int(raised.Int32)-current); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the run budget raise")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to raise the run budget")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"max_runs": raised.Int32, "previous_max_runs": current})
}
