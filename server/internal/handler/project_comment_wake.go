package handler

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// SetProjectCommentWake sets (or, with null, clears) the project tier of the
// comment_wake setting (CHE-5907). Same access rule as the project's workflow
// (canManageProjectWorkflow). The audit row lands in the write's transaction.
func (h *Handler) SetProjectCommentWake(w http.ResponseWriter, r *http.Request) {
	project, member, workspaceID, ok := h.loadProjectForWorkflowWrite(w, r)
	if !ok {
		return
	}
	var req struct {
		CommentWake *string `json:"comment_wake"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CommentWake != nil && !validCommentWake(*req.CommentWake) {
		writeError(w, http.StatusBadRequest, commentWakeSetting+" must be "+commentWakeConversation+" or "+commentWakeMentionedOnly)
		return
	}
	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save comment_wake")
		return
	}
	defer tx.Rollback(ctx)
	// Lock the row so the audit row records the value this write replaces.
	var prev []byte
	if err := tx.QueryRow(ctx, `SELECT settings FROM project WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, project.ID, project.WorkspaceID).Scan(&prev); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save comment_wake")
		return
	}
	qtx := h.Queries.WithTx(tx)
	var value pgtype.Text
	if req.CommentWake != nil {
		value = pgtype.Text{String: *req.CommentWake, Valid: true}
	}
	updated, err := qtx.SetProjectCommentWake(ctx, db.SetProjectCommentWakeParams{ID: project.ID, WorkspaceID: project.WorkspaceID, CommentWake: value})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save comment_wake")
		return
	}
	const kind = "project.comment_wake.update"
	if err := h.writeAudit(ctx, qtx, r, auditInput{
		Kind: kind, WorkspaceID: project.WorkspaceID, TargetID: project.ID,
		Before: map[string]any{commentWakeSetting: commentWakeFrom(prev)},
		After:  map[string]any{commentWakeSetting: commentWakeFrom(updated.Settings)},
	}); err != nil {
		writeAuditFailure(w, r, err, kind)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save comment_wake")
		return
	}
	writeJSON(w, http.StatusOK, h.publishProjectWorkflowUpdate(ctx, workspaceID, member, updated))
}
