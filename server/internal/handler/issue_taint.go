package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Issue-level taint (CHE-2216 plan 3.7). Taint reaches issue_taint through
// database triggers (migration 866); only a member clears it, here.

// lockedIssueTaint reads the issue's active taint IDs under a share lock on the
// issue row, in a second statement so it sees what a writer committed while
// the lock was awaited. Lock order everywhere a run of an issue is read, inserted
// or updated: the issue row first, then task_elevation, then the task row.
func lockedIssueTaint(ctx context.Context, q *db.Queries, issueID pgtype.UUID) ([]string, error) {
	if err := q.LockIssueForTaint(ctx, issueID); err != nil {
		return nil, err
	}
	return q.GetIssueTaintIDs(ctx, issueID)
}

type clearIssueTaintRequest struct {
	TaintIDs []string `json:"taint_ids"`
}

// ClearIssueTaint removes taint IDs from one issue and records who cleared
// them and when. No IDs named clears every active one. Machine credentials
// never reach it (route guard) and are refused again here.
func (h *Handler) ClearIssueTaint(w http.ResponseWriter, r *http.Request) {
	actorType, _ := h.resolveActor(r, requestUserID(r), h.resolveWorkspaceID(r))
	if isGovernedFieldMachineActor(r, actorType) {
		writeError(w, http.StatusForbidden, "only a person can clear issue taint")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	member, ok := h.requireWorkspaceRole(w, r, uuidToString(issue.WorkspaceID), "workspace not found", "owner", "admin")
	if !ok {
		return
	}
	var req clearIssueTaintRequest
	if !decodeStrict(w, r, &req) {
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear the taint")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if err := qtx.LockIssueForTaintClear(r.Context(), issue.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear the taint")
		return
	}
	// Fence next: an elevation that predates the taint must not outlive it.
	if _, err := qtx.FenceIssueTaskElevations(r.Context(), issue.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear the taint")
		return
	}
	active, err := qtx.GetIssueTaintIDs(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear the taint")
		return
	}
	if len(active) == 0 {
		writeError(w, http.StatusConflict, "issue is not tainted")
		return
	}
	cleared := active
	if len(req.TaintIDs) > 0 {
		cleared = slices.Compact(slices.Sorted(slices.Values(req.TaintIDs)))
		if slices.ContainsFunc(cleared, func(id string) bool { return !slices.Contains(active, id) }) {
			writeError(w, http.StatusBadRequest, "taint_ids must be active taint IDs of this issue")
			return
		}
	}
	if err := qtx.ClearIssueTaintIDs(r.Context(), db.ClearIssueTaintIDsParams{IssueID: issue.ID, Cleared: cleared}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear the taint")
		return
	}
	row, err := qtx.InsertIssueTaintClear(r.Context(), db.InsertIssueTaintClearParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaintIds: cleared, ClearedByMemberID: member.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the clear")
		return
	}
	details, _ := json.Marshal(map[string]any{"clear_id": uuidToString(row.ID), "taint_ids": cleared})
	if _, err := qtx.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "member", Valid: true},
		ActorID: member.UserID, Action: "issue_taint_cleared", Details: details,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the clear")
		return
	}
	remaining := slices.DeleteFunc(slices.Clone(active), func(id string) bool { return slices.Contains(cleared, id) })
	if err := tx.Commit(r.Context()); err != nil {
		slog.Warn("clear issue taint commit failed", "issue_id", uuidToString(issue.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to clear the taint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared, "remaining": remaining})
}
