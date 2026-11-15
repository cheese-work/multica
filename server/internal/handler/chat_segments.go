package handler

import (
	"bytes"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2819 L2a. A compacted DM keeps its old messages, but an agent reading
// with a task token sees only the current segment (Rule P: a message is in it
// iff (created_at, id) > (segment_floor_at, segment_floor_id)). Old segments
// reach an agent only through the explicit segment reads below.

// agentSegmentFloor is the floor to apply to a message read: the session's own
// for an agent task token, none for a signed-in member or a never-compacted DM.
// Zero values mean "no floor" to the SQL (both narg params NULL).
func agentSegmentFloor(session db.ChatSession, actorSource string) (pgtype.Timestamptz, pgtype.UUID) {
	if actorSource != "task_token" || !session.SegmentFloorID.Valid {
		return pgtype.Timestamptz{}, pgtype.UUID{}
	}
	return session.SegmentFloorAt, session.SegmentFloorID
}

// afterSegmentFloor drops the rows at or before the floor from an unpaged list.
// A zero floor keeps everything. Same order as SQL: (created_at, id), the id
// compared bytewise like Postgres uuid.
func afterSegmentFloor(msgs []db.ChatMessage, floorAt pgtype.Timestamptz, floorID pgtype.UUID) []db.ChatMessage {
	if !floorID.Valid {
		return msgs
	}
	out := msgs[:0:0]
	for _, m := range msgs {
		if m.CreatedAt.Time.After(floorAt.Time) ||
			(m.CreatedAt.Time.Equal(floorAt.Time) && bytes.Compare(m.ID.Bytes[:], floorID.Bytes[:]) > 0) {
			out = append(out, m)
		}
	}
	return out
}

type ChatSegmentResponse struct {
	DividerID    string `json:"divider_id"`
	CreatedAt    string `json:"created_at"`
	MessageCount int    `json:"message_count"`
}

type ChatSegmentsResponse struct {
	Segments []ChatSegmentResponse `json:"segments"`
}

// ListChatSegments serves GET /api/chat/sessions/{id}/segments: the closed
// segments of the caller's own DM, oldest first, named by their divider id.
func (h *Handler) ListChatSegments(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	session, ok := h.chatReadSession(w, r, userID)
	if !ok {
		return
	}
	rows, err := h.Queries.ListChatSegments(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat segments")
		return
	}
	resp := ChatSegmentsResponse{Segments: make([]ChatSegmentResponse, len(rows))}
	for i, row := range rows {
		resp.Segments[i] = ChatSegmentResponse{
			DividerID:    uuidToString(row.DividerID),
			CreatedAt:    row.CreatedAt.Time.Format(time.RFC3339Nano),
			MessageCount: int(row.MessageCount),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetChatSegment serves GET /api/chat/sessions/{id}/segment?divider=<id>: one
// closed segment of the caller's own DM, newest page first like the message
// page. A divider of another DM, or an id that is not a divider, is a 404.
func (h *Handler) GetChatSegment(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	session, ok := h.chatReadSession(w, r, userID)
	if !ok {
		return
	}
	dividerID, ok := parseUUIDOrBadRequest(w, r.URL.Query().Get("divider"), "divider")
	if !ok {
		return
	}
	limit, beforeCreatedAt, beforeID, err := parseChatMessagesPageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	divider, err := h.Queries.GetChatMessage(r.Context(), dividerID)
	if err != nil || divider.ChatSessionID != session.ID || divider.MessageKind != protocol.ChatMessageKindCompactionSummary {
		writeError(w, http.StatusNotFound, "segment not found")
		return
	}
	messages, err := h.Queries.ListChatSegmentMessagesPage(r.Context(), db.ListChatSegmentMessagesPageParams{
		ChatSessionID:   session.ID,
		EndCreatedAt:    divider.CreatedAt,
		EndID:           divider.ID,
		BeforeCreatedAt: beforeCreatedAt,
		BeforeID:        beforeID,
		RowLimit:        int32(limit + 1),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat segment")
		return
	}
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	var nextCursor *ChatMessagesCursorResponse
	if hasMore {
		oldest := messages[len(messages)-1]
		nextCursor = &ChatMessagesCursorResponse{
			CreatedAt: oldest.CreatedAt.Time.Format(time.RFC3339Nano),
			ID:        uuidToString(oldest.ID),
		}
	}
	// SQL is newest first; the page is chronological.
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	writeJSON(w, http.StatusOK, ChatMessagesPageResponse{
		Messages:   h.chatMessageResponses(r.Context(), ctxWorkspaceID(r.Context()), messages),
		Limit:      limit,
		HasMore:    hasMore,
		NextCursor: nextCursor,
	})
}
