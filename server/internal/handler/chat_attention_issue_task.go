package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// staleDMResponse is everything an issue task sees of a stale DM: the fields
// `multica chat attention --stale` prints. No message text, no agent, no creator.
type staleDMResponse struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Status          string  `json:"status"`
	StaleLevel      string  `json:"stale_level"`
	AttentionSource *string `json:"attention_source"`
	AttentionSince  *string `json:"attention_since"`
	NextChangeAt    *string `json:"next_change_at"`
}

// listRequesterStaleDMs answers GET /api/chat/sessions for a task token that
// runs an issue (not a chat), CHE-6360. It lists the stale (yellow or red) DMs
// of that issue's requester and no one else's, read-only, so the morning brief
// can run `multica chat attention --stale`. It reports true when it wrote the
// response; false leaves the caller on the own-chat-session path, so chat tasks
// and tokens without an issue keep their behavior.
//
// The person comes only from issue.requested_by_id of the task's own issue,
// never from the request: no query or header names another person.
func (h *Handler) listRequesterStaleDMs(w http.ResponseWriter, r *http.Request, workspaceID string) bool {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		return false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		return false
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil || task.ChatSessionID.Valid || !task.IssueID.Valid {
		return false
	}
	issue, err := h.Queries.GetIssue(r.Context(), task.IssueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		writeError(w, http.StatusInternalServerError, "failed to load task issue")
		return true
	}
	if uuidToString(issue.WorkspaceID) != workspaceID {
		writeError(w, http.StatusForbidden, "task does not belong to this workspace")
		return true
	}
	if !issue.RequestedByID.Valid {
		writeError(w, http.StatusForbidden, "this issue has no requester; stale DMs are read for the issue's requester only")
		return true
	}
	requester := uuidToString(issue.RequestedByID)
	member, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: issue.RequestedByID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusForbidden, "the issue's requester is not a member of this workspace")
		return true
	}
	// The requester's own agent access, as the interactive list applies it.
	allowed, ok := h.accessibleAgentIDs(r.Context(), workspaceID, "member", requester, member.Role)
	if !ok {
		writeError(w, http.StatusInternalServerError, "failed to resolve agent access")
		return true
	}
	rows, err := h.Queries.ListChatSessionsByCreator(r.Context(), db.ListChatSessionsByCreatorParams{WorkspaceID: issue.WorkspaceID, CreatorID: issue.RequestedByID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chat sessions")
		return true
	}
	attention, err := service.LoadChatAttention(r.Context(), h.Queries, issue.WorkspaceID, issue.RequestedByID, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load chat attention")
		return true
	}
	byCanonical := make(map[string]service.ChatAttention, len(attention))
	for _, a := range attention {
		byCanonical[a.CanonicalID] = a
	}
	resp := []staleDMResponse{}
	for _, s := range rows {
		a, has := byCanonical[uuidToString(s.ID)]
		if !has || (a.StaleLevel != service.AttentionYellow && a.StaleLevel != service.AttentionRed) {
			continue
		}
		if _, ok := allowed[uuidToString(s.AgentID)]; !ok {
			continue
		}
		resp = append(resp, staleDMResponse{
			ID: uuidToString(s.ID), Title: s.Title, Status: s.Status, StaleLevel: a.StaleLevel,
			AttentionSource: a.AttentionSource, AttentionSince: formatAttentionTime(a.AttentionSince), NextChangeAt: formatAttentionTime(a.NextChangeAt),
		})
	}
	writeJSON(w, http.StatusOK, resp)
	return true
}
