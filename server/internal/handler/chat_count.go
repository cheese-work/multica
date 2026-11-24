package handler

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// chatCountKinds maps the `kind` a caller names to the chat_message.message_kind
// it counts. Only kinds listed here can be counted: the endpoint is for metrics,
// not a general message reader (CHE-9364).
var chatCountKinds = map[string]string{
	"progress-card": protocol.ChatMessageKindProgressCard,
}

type chatCountResponse struct {
	Count int       `json:"count"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// GetChatCount serves `multica chat count` (CHE-9364): how many messages of one
// kind a member's DMs received in [since, until). It returns the count and the
// window only — never a message body, a session id or another member's data —
// so flow metric 7 can read "Progress update" cards without a path into the DM.
//
// Access (the PR states the same rule):
//   - an interactive member counts their own DMs only;
//   - a task token counts on behalf of ?member only when that run is the
//     member's: the run's user is the member, or the calling agent is owned by
//     the member, and the member is in the token's workspace;
//   - a CLI login (cli_jwt), a PAT, or any other source is refused.
//
// Archived DMs count: the card was delivered. since is required; until defaults
// to now.
func (h *Handler) GetChatCount(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	caller, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	q := r.URL.Query()
	messageKind, known := chatCountKinds[q.Get("kind")]
	if !known {
		writeError(w, http.StatusBadRequest, "invalid kind")
		return
	}
	memberUserID, ok := parseUUIDOrBadRequest(w, q.Get("member"), "member")
	if !ok {
		return
	}
	since, err := time.Parse(time.RFC3339, q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "since must be an RFC3339 time")
		return
	}
	until := time.Now().UTC()
	if raw := q.Get("until"); raw != "" {
		if until, err = time.Parse(time.RFC3339, raw); err != nil {
			writeError(w, http.StatusBadRequest, "until must be an RFC3339 time")
			return
		}
	}
	if !until.After(since) {
		writeError(w, http.StatusBadRequest, "until must be after since")
		return
	}

	switch r.Header.Get("X-Actor-Source") {
	case "interactive_jwt":
		if memberUserID != caller.UserID {
			writeError(w, http.StatusForbidden, "members count only their own chats")
			return
		}
	case "task_token":
		if !h.taskTokenActsForMember(r, caller.UserID, memberUserID) {
			writeError(w, http.StatusForbidden, "this run does not act for that member")
			return
		}
	default:
		writeError(w, http.StatusForbidden, "interactive authentication or an agent task is required")
		return
	}

	var count int
	if err := h.DB.QueryRow(r.Context(), `
		SELECT count(*)
		FROM chat_message cm
		JOIN chat_session cs ON cs.id = cm.chat_session_id
		WHERE cs.workspace_id = $1 AND cs.creator_id = $2
		  AND cm.message_kind = $3 AND cm.created_at >= $4 AND cm.created_at < $5`,
		caller.WorkspaceID, memberUserID, messageKind, since, until).Scan(&count); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count chat messages")
		return
	}
	writeJSON(w, http.StatusOK, chatCountResponse{Count: count, Since: since.UTC(), Until: until.UTC()})
}

// taskTokenActsForMember reports whether a task-token run acts for member: the
// run's user is the member, or the calling agent is owned by a current member of
// the token's workspace who is the named one. runUser is already a verified
// workspace member (requireWorkspaceMember).
func (h *Handler) taskTokenActsForMember(r *http.Request, runUser, member pgtype.UUID) bool {
	if runUser == member {
		return true
	}
	agentID, err := util.ParseUUID(r.Header.Get("X-Agent-ID"))
	if err != nil {
		return false
	}
	agent, err := h.Queries.GetAgent(r.Context(), agentID)
	if err != nil || !agent.OwnerID.Valid || agent.OwnerID != member {
		return false
	}
	return h.isActiveWorkspaceMember(r.Context(), uuidToString(member), ctxWorkspaceID(r.Context()))
}
