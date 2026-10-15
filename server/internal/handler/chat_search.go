package handler

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	chatSearchDefaultLimit = 20
	chatSearchMaxLimit     = 50
	chatSearchMaxQueryLen  = 200
)

// chatSearchSQL finds the caller's DMs by title or message text. $1 workspace,
// $2 user, $3 raw query, $4 escaped LIKE pattern, $5 limit, $6 offset.
//
// The readable CTE restates chatReadSession as predicates and must stay in
// step with it: caller is the creator and a workspace member (gateChat-
// SessionForUser + canAccessPrivateAgent), the session is public
// (GetPublicChatSessionInWorkspace), and the agent is the caller's, the
// caller is owner/admin, or it is public_to with a workspace or member target
// that admits the caller. The msg lateral repeats the message visibility
// filter of ListChatMessagesPage; hidden rows must never match, or the snippet
// and the anchor id would leak them. It runs per readable session (newest
// first, stopping at the first match) so the cost follows the caller's own
// DMs, not the size of chat_message.
//
// The snippet window starts 40 characters before the first case-insensitive
// match, found by regex in the original text. Positions from LOWER(content)
// are not reused: under an ICU database LOWER can change the character count
// and shift the window. The LOWER position is only the fallback for a match
// the regex does not fold the same way.
const chatSearchSQL = `
WITH readable AS (
  SELECT cs.id, cs.title, cs.agent_id, cs.status, cs.updated_at
  FROM chat_session AS cs
  JOIN member AS mem ON mem.workspace_id = cs.workspace_id AND mem.user_id = $2
  JOIN agent AS a ON a.id = cs.agent_id
  WHERE cs.workspace_id = $1
    AND cs.creator_id = $2
    AND (
      cs.explicitly_created_at IS NOT NULL
      OR EXISTS (
        SELECT 1 FROM chat_message AS public_message
        WHERE public_message.chat_session_id = cs.id
          AND public_message.message_kind != 'channel_command'
      )
    )
    AND (
      a.owner_id = $2
      OR mem.role IN ('owner', 'admin')
      OR (
        a.permission_mode = 'public_to'
        AND EXISTS (
          SELECT 1 FROM agent_invocation_target AS target
          WHERE target.agent_id = a.id
            AND (target.target_type = 'workspace'
                 OR (target.target_type = 'member' AND target.target_id = $2))
        )
      )
    )
)
SELECT readable.id, readable.title, readable.agent_id, readable.status, readable.updated_at,
       msg.id,
       CASE WHEN msg.id IS NULL THEN ''
            ELSE regexp_replace(
                   substr(msg.content, GREATEST(COALESCE(
                     length((regexp_match(msg.content, '^(.*?)' || regexp_replace($3, '[\\.^$|?*+()\[\]{}]', '\\\&', 'g'), 'i'))[1]),
                     strpos(LOWER(msg.content), LOWER($3)) - 1) + 1 - 40, 1), 160),
                   '\s+', ' ', 'g')
       END
FROM readable
LEFT JOIN LATERAL (
  SELECT message.id, message.content, message.created_at
  FROM chat_message AS message
  WHERE message.chat_session_id = readable.id
    AND LOWER(message.content) LIKE LOWER($4)
    AND message.message_kind NOT IN ('channel_command', 'onboarding_kickoff')
    AND NOT (
      message.role = 'user'
      AND EXISTS (
        SELECT 1
        FROM agent_task_queue AS task
        WHERE task.chat_session_id = message.chat_session_id
          AND task.status = 'queued'
          AND task.id = message.task_id
          AND task.id <> (
            SELECT head.id
            FROM agent_task_queue AS head
            WHERE head.chat_session_id = message.chat_session_id
              AND head.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
              AND head.regenerate_quick_actions_for IS NULL
            ORDER BY
              CASE
                WHEN head.status IN ('dispatched', 'running', 'waiting_local_directory') THEN 0
                WHEN head.status = 'deferred' THEN 1
                ELSE 2
              END,
              head.priority DESC,
              head.created_at ASC,
              head.id ASC
            LIMIT 1
          )
      )
    )
  ORDER BY message.created_at DESC, message.id DESC
  LIMIT 1
) AS msg ON TRUE
WHERE LOWER(readable.title) LIKE LOWER($4) OR msg.id IS NOT NULL
ORDER BY (LOWER(readable.title) LIKE LOWER($4)) DESC,
         COALESCE(msg.created_at, readable.updated_at) DESC,
         readable.id
LIMIT $5 OFFSET $6`

type ChatSearchHit struct {
	SessionID string  `json:"session_id"`
	Title     string  `json:"title"`
	AgentID   string  `json:"agent_id"`
	MessageID *string `json:"message_id"`
	Snippet   string  `json:"snippet"`
	UpdatedAt string  `json:"updated_at"`
	Archived  bool    `json:"archived"`
}

type ChatSearchResponse struct {
	Results    []ChatSearchHit `json:"results"`
	NextCursor *string         `json:"next_cursor,omitempty"`
}

// SearchChats serves GET /api/chat/search for interactive users only: task and
// CLI callers have no DM inbox to search.
func (h *Handler) SearchChats(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if r.Header.Get("X-Actor-Source") != "interactive_jwt" {
		writeError(w, http.StatusForbidden, "interactive authentication is required")
		return
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(w, ctxWorkspaceID(r.Context()), "workspace id")
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" || utf8.RuneCountInString(query) > chatSearchMaxQueryLen {
		writeError(w, http.StatusBadRequest, "q must be 1-"+strconv.Itoa(chatSearchMaxQueryLen)+" characters")
		return
	}
	limit := chatSearchDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = min(v, chatSearchMaxLimit)
		}
	}
	// ponytail: the cursor is a plain offset; keyset paging if results shift
	// between pages often enough to matter.
	offset := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		offset = v
	}

	args := []any{workspaceUUID, parseUUID(userID), query, "%" + escapeLike(query) + "%", limit + 1, offset}
	results := make([]ChatSearchHit, 0, limit+1)
	err := runSearchQuery(r.Context(), h.TxStarter, chatSearchSQL, args, func(rows pgx.Rows) error {
		for rows.Next() {
			var (
				sessionID, agentID, messageID pgtype.UUID
				title, status, snippet        string
				updatedAt                     pgtype.Timestamptz
			)
			if err := rows.Scan(&sessionID, &title, &agentID, &status, &updatedAt, &messageID, &snippet); err != nil {
				return err
			}
			results = append(results, ChatSearchHit{
				SessionID: uuidToString(sessionID),
				Title:     title,
				AgentID:   uuidToString(agentID),
				MessageID: uuidToPtr(messageID),
				Snippet:   snippet,
				UpdatedAt: timestampToString(updatedAt),
				Archived:  status == "archived",
			})
		}
		return rows.Err()
	})
	if err != nil {
		if isSearchStatementTimeout(err) {
			slog.Warn("search chats timed out", "workspace_id", uuidToString(workspaceUUID), "timeout", searchStatementTimeout)
			writeError(w, http.StatusServiceUnavailable, "search timed out; please refine your query or try again")
			return
		}
		slog.Warn("search chats failed", "error", err, "workspace_id", uuidToString(workspaceUUID))
		writeError(w, http.StatusInternalServerError, "failed to search chats")
		return
	}

	resp := ChatSearchResponse{Results: results}
	if len(results) > limit {
		resp.Results = results[:limit]
		next := strconv.Itoa(offset + limit)
		resp.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

// listChatMessagesAround serves ?around=<message_id>: a window centred on the
// anchor, oldest first. The caller has already passed chatReadSession. An
// anchor that is missing, in another session or hidden from members is a 404.
func (h *Handler) listChatMessagesAround(w http.ResponseWriter, r *http.Request, session db.ChatSession, workspaceID, rawAnchor string) {
	anchorID, ok := parseUUIDOrBadRequest(w, rawAnchor, "around")
	if !ok {
		return
	}
	limit, beforeCreatedAt, _, err := parseChatMessagesPageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if beforeCreatedAt.Valid {
		writeError(w, http.StatusBadRequest, "around cannot be combined with a cursor")
		return
	}
	anchor, err := h.Queries.GetChatMessage(r.Context(), anchorID)
	if err != nil || anchor.ChatSessionID != session.ID {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}

	// Newer half includes the anchor; the extra row only detects has_newer.
	olderLimit := limit / 2
	newerLimit := limit - olderLimit
	newer, err := h.Queries.ListChatMessagesNewerPage(r.Context(), db.ListChatMessagesNewerPageParams{
		ChatSessionID:  session.ID,
		Limit:          int32(newerLimit + 1),
		AfterCreatedAt: anchor.CreatedAt,
		AfterID:        anchor.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat messages")
		return
	}
	if len(newer) == 0 || newer[0].ID != anchor.ID {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	hasNewer := len(newer) > newerLimit
	if hasNewer {
		newer = newer[:newerLimit]
	}

	// Two extra rows absorb the kickoff row that visibleChatMessages may drop.
	older, err := h.Queries.ListChatMessagesPage(r.Context(), db.ListChatMessagesPageParams{
		ChatSessionID:   session.ID,
		Limit:           int32(olderLimit + 2),
		BeforeCreatedAt: anchor.CreatedAt,
		BeforeID:        anchor.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat messages")
		return
	}
	older = visibleChatMessages(older)
	hasMore := len(older) > olderLimit
	if hasMore {
		older = older[:olderLimit]
	}
	// SQL returns older rows newest first; the window is chronological.
	window := make([]db.ChatMessage, 0, len(older)+len(newer))
	for i := len(older) - 1; i >= 0; i-- {
		window = append(window, older[i])
	}
	window = append(window, newer...)

	var nextCursor *ChatMessagesCursorResponse
	if hasMore {
		nextCursor = &ChatMessagesCursorResponse{
			CreatedAt: window[0].CreatedAt.Time.Format(time.RFC3339Nano),
			ID:        uuidToString(window[0].ID),
		}
	}
	writeJSON(w, http.StatusOK, ChatMessagesPageResponse{
		Messages:   h.chatMessageResponses(r.Context(), workspaceID, window),
		Limit:      limit,
		HasMore:    hasMore,
		NextCursor: nextCursor,
		HasNewer:   hasNewer,
	})
}
