package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

// SilenceAlertRequest is the daemon watchdog's report that a running task has
// produced no output for silent_for_seconds. segment_id names one continuous
// silence, so a replayed report for the same segment is a no-op.
type SilenceAlertRequest struct {
	SegmentID        string `json:"segment_id"`
	SilentForSeconds int    `json:"silent_for_seconds"`
}

// ReportSilenceAlert leaves one system comment on the task's issue.
//
// Idempotency lives in silence_alert_receipt, not in the comment: an admin may
// delete the comment, and the replay must not bring it back. The alert is
// advisory, so every failure past the access check answers 200 with a log line;
// a 5xx here would make the daemon treat a diagnostic as a run error.
func (h *Handler) ReportSilenceAlert(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req SilenceAlertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SegmentID == "" || len(req.SegmentID) > 512 {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	task, scope, ok := h.requireDaemonTaskAccessWithScope(w, r, taskID)
	if !ok {
		return
	}
	ack := func() { writeJSON(w, http.StatusOK, map[string]string{"status": "ok"}) }

	if !task.IssueID.Valid {
		ack()
		return
	}
	issue, err := h.Queries.GetIssue(r.Context(), task.IssueID)
	if err != nil {
		slog.Warn("silence alert: load issue failed", "task_id", taskID, "error", err)
		ack()
		return
	}

	created, posted := h.postSilenceAlert(r.Context(), task.ID, issue, req)
	if !posted {
		ack()
		return
	}

	comment := created.Comment()
	h.Bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: scope.WorkspaceID,
		ActorType:   "system",
		Payload: map[string]any{
			"comment": map[string]any{
				"id":          util.UUIDToString(comment.ID),
				"issue_id":    util.UUIDToString(comment.IssueID),
				"author_type": comment.AuthorType,
				"author_id":   util.UUIDToString(comment.AuthorID),
				"content":     comment.Content,
				"type":        comment.Type,
				"revision":    comment.Revision,
			},
			"issue_title":    issue.Title,
			"issue_revision": created.IssueRevision,
		},
	})
	ack()
}

// postSilenceAlert reports whether it created the comment. The task row is
// locked FOR SHARE in the same transaction as the receipt and the comment, so a
// completion either commits first (no active task, no alert) or waits for the
// alert; the comment never claims a finished run is still active.
func (h *Handler) postSilenceAlert(ctx context.Context, taskID pgtype.UUID, issue db.Issue, req SilenceAlertRequest) (db.CreateCommentRow, bool) {
	tid := util.UUIDToString(taskID)
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Warn("silence alert: begin failed", "task_id", tid, "error", err)
		return db.CreateCommentRow{}, false
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	qtx := h.Queries.WithTx(tx)

	if _, err := qtx.LockActiveTaskForSilenceAlert(ctx, taskID); err != nil {
		if !isNotFound(err) {
			slog.Warn("silence alert: lock task failed", "task_id", tid, "error", err)
		}
		return db.CreateCommentRow{}, false
	}
	claimed, err := qtx.ClaimSilenceAlertReceipt(ctx, db.ClaimSilenceAlertReceiptParams{TaskID: taskID, SegmentID: req.SegmentID})
	if err != nil {
		slog.Warn("silence alert: claim receipt failed", "task_id", tid, "error", err)
		return db.CreateCommentRow{}, false
	}
	if claimed == 0 {
		return db.CreateCommentRow{}, false
	}
	created, err := qtx.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content: redact.Prose(fmt.Sprintf(
			"The agent has produced no output for %d minutes. The run is still active.",
			max(req.SilentForSeconds, 0)/60,
		)),
		Type:         "system",
		SourceTaskID: taskID,
	})
	if err != nil {
		slog.Warn("silence alert: create system comment failed", "task_id", tid, "error", err)
		return db.CreateCommentRow{}, false
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Warn("silence alert: commit failed", "task_id", tid, "error", err)
		return db.CreateCommentRow{}, false
	}
	return created, true
}
