package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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
// Idempotency lives in the comment id: it is derived from (task, segment), so a
// replay collides on the primary key and posts nothing — no extra table. The
// alert is advisory, so every failure past the access check answers 200 with a
// log line; a 5xx here would make the daemon treat a diagnostic as a run error.
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

	switch task.Status {
	case "dispatched", "waiting_local_directory", "running":
	default:
		ack()
		return
	}
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

	commentID := uuid.NewSHA1(uuid.UUID(task.ID.Bytes), []byte("silence-alert:"+req.SegmentID))
	content := redact.Prose(fmt.Sprintf(
		"The agent has produced no output for %d minutes. The run is still active.",
		max(req.SilentForSeconds, 0)/60,
	))
	created, err := h.Queries.CreateComment(r.Context(), db.CreateCommentParams{
		ID:           pgtype.UUID{Bytes: commentID, Valid: true},
		IssueID:      issue.ID,
		WorkspaceID:  issue.WorkspaceID,
		AuthorType:   "system",
		AuthorID:     pgtype.UUID{Valid: true},
		Content:      content,
		Type:         "system",
		SourceTaskID: task.ID,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			slog.Warn("silence alert: create system comment failed", "task_id", taskID, "error", err)
		}
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
