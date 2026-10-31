package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// TopUpIssueRunBudget opens a new run-budget window on the issue without a
// status move (CHE-2297 L2d): the window credit is the cap in force, and the
// token allowance follows once token accounting is active. Members only, for
// the same reason as a raise: an agent cannot pay for its own runs. The
// activity row is the window's start, so it commits with the credit.
func (h *Handler) TopUpIssueRunBudget(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	actorType, actorID := h.resolveActor(r, userID, uuidToString(issue.WorkspaceID))
	// resolveActor calls a machine credential without agent headers a member, so
	// the credential check is separate (the route carries RequireHumanActor too).
	if actorType != "member" || isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "only a person can top up an issue's run budget")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to top up the run budget")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: actorType, Valid: true},
		ActorID: parseUUID(actorID), Action: "run_budget_top_up", Details: []byte("{}"),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the run budget top-up")
		return
	}
	max, err := service.OpenRunBudgetWindow(r.Context(), qtx, issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to top up the run budget")
		return
	}
	if max == 0 {
		writeError(w, http.StatusBadRequest, "the issue has no workflow run budget to top up")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to top up the run budget")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"max_runs": max})
}

// openRunBudgetWindowForMove opens the run-budget window a person's status move
// started, so its token allowance is claimed when the move is recorded rather
// than at the next admission or report. It runs after the event is published:
// the activity listener has written the move by then, and the window begins at
// that row. A failure is logged; the next admission or report claims the same
// allowance.
func (h *Handler) openRunBudgetWindowForMove(ctx context.Context, issue db.Issue) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Warn("run budget: open window for status move", "issue_id", uuidToString(issue.ID), "error", err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err := service.OpenRunBudgetWindow(ctx, h.Queries.WithTx(tx), issue.ID); err != nil {
		slog.Warn("run budget: open window for status move", "issue_id", uuidToString(issue.ID), "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Warn("run budget: open window for status move", "issue_id", uuidToString(issue.ID), "error", err)
	}
}

// inRunBudgetTx runs a ledger write in its own transaction. A failure is
// logged and the caller's work stands: only the credit is missed, and the next
// event writes the same keys.
func (h *Handler) inRunBudgetTx(ctx context.Context, issueID pgtype.UUID, what string, fn func(*db.Queries) error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err == nil {
		defer tx.Rollback(ctx)
		if err = fn(h.Queries.WithTx(tx)); err == nil {
			err = tx.Commit(ctx)
		}
	}
	if err != nil {
		slog.Warn("run budget: "+what, "issue_id", uuidToString(issueID), "error", err)
	}
}

// grantRunBudgetStep grants the first entry of a status in the budget window
// when an agent or a workflow edge moved the issue. It runs after the event is
// published, like openRunBudgetWindowForMove: the activity listener has written
// the move, and the window's first status is read from it.
func (h *Handler) grantRunBudgetStep(ctx context.Context, issue db.Issue, prevStatus string) {
	h.inRunBudgetTx(ctx, issue.ID, "grant step", func(q *db.Queries) error {
		return service.GrantRunBudgetStep(ctx, q, issue, prevStatus, false)
	})
}

func (h *Handler) grantRunBudgetHead(ctx context.Context, issueID, prID pgtype.UUID, sha string) {
	h.inRunBudgetTx(ctx, issueID, "grant head", func(q *db.Queries) error {
		return service.GrantRunBudgetHead(ctx, q, issueID, prID, sha)
	})
}

func (h *Handler) refundSupersededRun(ctx context.Context, task db.AgentTaskQueue) {
	h.inRunBudgetTx(ctx, task.IssueID, "refund superseded run", func(q *db.Queries) error {
		return service.RefundSupersededRun(ctx, q, task)
	})
}
