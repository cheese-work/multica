package handler

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// markCarriedReplies sets CarriedFromSessionID on assistant rows whose run ran
// in another DM. A rollover writes the run's final reply into the successor DM
// and keeps its task_id (CHE-2119); taskSessions maps task id to the DM it ran in.
func markCarriedReplies(rows []ChatMessageResponse, taskSessions map[string]string) {
	for i := range rows {
		row := &rows[i]
		if row.Role != "assistant" || row.TaskID == nil {
			continue
		}
		if origin, ok := taskSessions[*row.TaskID]; ok && origin != row.ChatSessionID {
			row.CarriedFromSessionID = &origin
		}
	}
}

// chatTaskSessions maps the assistant rows' task ids to the DM each task ran in.
// A failed lookup only means replies show their steps, so it is logged, not fatal.
func (h *Handler) chatTaskSessions(ctx context.Context, messages []db.ChatMessage) map[string]string {
	var taskIDs []pgtype.UUID
	for _, m := range messages {
		if m.Role == "assistant" && m.TaskID.Valid {
			taskIDs = append(taskIDs, m.TaskID)
		}
	}
	if len(taskIDs) == 0 {
		return nil
	}
	rows, err := h.DB.Query(ctx, `SELECT id, chat_session_id FROM agent_task_queue WHERE id = ANY($1::uuid[])`, taskIDs)
	if err != nil {
		slog.Warn("list task sessions for chat messages failed", "error", err)
		return nil
	}
	defer rows.Close()
	sessions := make(map[string]string, len(taskIDs))
	for rows.Next() {
		var taskID, sessionID pgtype.UUID
		if err := rows.Scan(&taskID, &sessionID); err != nil {
			slog.Warn("scan task session for chat messages failed", "error", err)
			return nil
		}
		sessions[uuidToString(taskID)] = uuidToString(sessionID)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("read task sessions for chat messages failed", "error", err)
		return nil
	}
	return sessions
}
