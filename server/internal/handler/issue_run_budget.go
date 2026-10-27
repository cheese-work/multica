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
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RaiseIssueRunBudgetRequest sets the issue's own run cap to Runs total.
type RaiseIssueRunBudgetRequest struct {
	Runs int `json:"runs"`
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
	raised, err := h.Queries.RaiseIssueMaxRuns(r.Context(), db.RaiseIssueMaxRunsParams{
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
	if _, err := h.Queries.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: actorType, Valid: true},
		ActorID: parseUUID(actorID), Action: "run_budget_raised", Details: body,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "raised the run budget but failed to record it")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"max_runs": raised.Int32, "previous_max_runs": current})
}
