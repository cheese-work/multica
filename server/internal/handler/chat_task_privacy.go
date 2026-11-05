package handler

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A chat session is creator-private, and so is everything its tasks produce:
// the transcript, the reply in result, and the failure text in error. Task
// lists stay workspace-wide because presence and workload views count every
// run, so they keep a foreign chat task's row and drop only its content.

// redactForeignChatTaskContent clears the content of every chat task in resp
// that viewerID did not start. tasks[i] must be the row behind resp[i]. A chat
// task whose session can no longer be resolved is treated as foreign.
//
// A background task has no chat_session_id; its owner is the creator of the DM
// it came from (source_chat_session_id). A bg:* row with neither pointer, such
// as one written before the column existed, cannot be resolved and is foreign.
func (h *Handler) redactForeignChatTaskContent(ctx context.Context, viewerID string, tasks []db.AgentTaskQueue, resp []AgentTaskResponse) error {
	seen := map[pgtype.UUID]bool{}
	var sessionIDs []pgtype.UUID
	for _, t := range tasks {
		if s := ownerSession(t); s.Valid && !seen[s] {
			seen[s] = true
			sessionIDs = append(sessionIDs, s)
		}
	}
	owners := map[pgtype.UUID]string{}
	if len(sessionIDs) > 0 {
		rows, err := h.Queries.ListChatSessionCreators(ctx, sessionIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			owners[row.ID] = uuidToString(row.CreatorID)
		}
	}
	for i, t := range tasks {
		session := ownerSession(t)
		if !session.Valid && !strings.HasPrefix(t.Purpose.String, "bg:") {
			continue
		}
		if owner, ok := owners[session]; ok && owner != "" && owner == viewerID {
			continue
		}
		resp[i].Result = nil
		resp[i].Error = nil
	}
	return nil
}

// ownerSession is the chat session whose creator owns the task's content:
// COALESCE(chat_session_id, source_chat_session_id).
func ownerSession(t db.AgentTaskQueue) pgtype.UUID {
	if t.ChatSessionID.Valid {
		return t.ChatSessionID
	}
	return t.SourceChatSessionID
}
