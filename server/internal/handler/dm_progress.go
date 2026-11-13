package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DM progress follows (CHE-3369 C1, I3). The lane comes from the caller, never
// from the request body: a member names a DM they created with
// ?chat_session_id=, an agent names none and gets the DM its own task answers
// in. Every refusal is a 403, so a probe learns nothing about other DMs.

type dmProgressFollowResponse struct {
	IssueID string   `json:"issue_id"`
	LaneID  string   `json:"lane_id"`
	Reasons []string `json:"reasons"`
	Muted   bool     `json:"muted"`
}

func dmProgressFollowToResponse(f db.DmProgressFollow) dmProgressFollowResponse {
	return dmProgressFollowResponse{IssueID: uuidToString(f.IssueID), LaneID: uuidToString(f.LineageID), Reasons: f.Reasons, Muted: f.MutedAt.Valid}
}

// progressLane resolves the lane the caller may write. Agents get follow only
// (agentMayWrite); mute, unmute and unfollow stay with the member.
func (h *Handler) progressLane(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, agentMayWrite bool) (service.DMLane, bool) {
	deny := func() (service.DMLane, bool) {
		writeError(w, http.StatusForbidden, "this chat is not yours to change progress follows in")
		return service.DMLane{}, false
	}
	session := r.URL.Query().Get("chat_session_id")
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(workspaceID))
	if actorType == "agent" {
		taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
		if !agentMayWrite || session != "" || err != nil {
			return deny()
		}
		task, err := h.Queries.GetAgentTaskInWorkspace(r.Context(), db.GetAgentTaskInWorkspaceParams{ID: taskID, WorkspaceID: workspaceID})
		if err != nil || uuidToString(task.AgentID) != actorID {
			return deny()
		}
		if lane, ok := service.DMLaneForTask(r.Context(), h.Queries, task, workspaceID); ok {
			return lane, true
		}
		return deny()
	}
	if session == "" {
		writeError(w, http.StatusBadRequest, "chat_session_id is required")
		return service.DMLane{}, false
	}
	sessionID, err := util.ParseUUID(session)
	if err != nil {
		return deny()
	}
	lane, ok := service.DMLaneForMember(r.Context(), h.Queries, sessionID, parseUUID(requestUserID(r)), workspaceID)
	if !ok {
		return deny()
	}
	return lane, true
}

func (h *Handler) FollowIssueProgress(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	lane, ok := h.progressLane(w, r, issue.WorkspaceID, true)
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to follow")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if !service.ProgressVisible(r.Context(), qtx, lane.Owner, issue) {
		writeError(w, http.StatusForbidden, "the chat owner cannot read this issue")
		return
	}
	// Explicit follow: no row back means it is already active.
	if _, _, err := service.AddProgressFollow(r.Context(), qtx, lane, issue, []string{service.ReasonManual}, issue.ID, pgtype.Timestamptz{}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to follow")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to follow")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issue_id": uuidToString(issue.ID), "lane_id": uuidToString(lane.Lineage), "followed": true})
}

var errUnfollowScope = errors.New("scope=root needs an issue that is a followed root")

// UnfollowIssueProgress ends a follow. ?scope=issue (default) ends this issue's
// follow with all its reasons. ?scope=root also removes what the rest of the
// lane inherited from this issue, and leaves any follow that still has another
// reason, such as an independent manual follow.
func (h *Handler) UnfollowIssueProgress(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope != "" && scope != "issue" && scope != "root" {
		writeError(w, http.StatusBadRequest, "scope must be issue or root")
		return
	}
	h.changeProgressFollow(w, r, func(q *db.Queries, issue, lane pgtype.UUID) (db.DmProgressFollow, error) {
		if scope == "root" {
			target, err := q.GetActiveDMProgressFollow(r.Context(), db.GetActiveDMProgressFollowParams{IssueID: issue, LineageID: lane})
			if err != nil {
				return db.DmProgressFollow{}, err
			}
			if !service.IsRoot(target) {
				return db.DmProgressFollow{}, errUnfollowScope
			}
			if _, err := q.DropInheritedDMProgressReason(r.Context(), db.DropInheritedDMProgressReasonParams{LineageID: lane, Reason: service.InheritedReason(issue)}); err != nil {
				return db.DmProgressFollow{}, err
			}
		}
		return q.UnfollowDMProgressFollow(r.Context(), db.UnfollowDMProgressFollowParams{IssueID: issue, LineageID: lane})
	})
}

func (h *Handler) MuteIssueProgress(w http.ResponseWriter, r *http.Request) {
	h.setProgressMuted(w, r, true)
}

func (h *Handler) UnmuteIssueProgress(w http.ResponseWriter, r *http.Request) {
	h.setProgressMuted(w, r, false)
}

func (h *Handler) setProgressMuted(w http.ResponseWriter, r *http.Request, muted bool) {
	h.changeProgressFollow(w, r, func(q *db.Queries, issue, lane pgtype.UUID) (db.DmProgressFollow, error) {
		return q.SetDMProgressFollowMuted(r.Context(), db.SetDMProgressFollowMutedParams{IssueID: issue, LineageID: lane, Muted: muted})
	})
}

// changeProgressFollow runs a member-only follow change under the lane lock, so
// it serializes with the dispatcher and with every other follow write.
func (h *Handler) changeProgressFollow(w http.ResponseWriter, r *http.Request, change func(q *db.Queries, issue, lane pgtype.UUID) (db.DmProgressFollow, error)) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	lane, ok := h.progressLane(w, r, issue.WorkspaceID, false)
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update follow")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	follow, err := func() (db.DmProgressFollow, error) {
		if _, err := qtx.LockDMProgressLane(r.Context(), lane.Lineage); err != nil {
			return db.DmProgressFollow{}, err
		}
		return change(qtx, issue.ID, lane.Lineage)
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no active follow for this issue in this chat")
		return
	}
	if errors.Is(err, errUnfollowScope) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		writeError(w, http.StatusInternalServerError, "failed to update follow")
		return
	}
	writeJSON(w, http.StatusOK, dmProgressFollowToResponse(follow))
}

// ListDMProgressFollows lists the active follows of a DM the caller created.
func (h *Handler) ListDMProgressFollows(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireUserID(w, r); !ok {
		return
	}
	workspaceID := parseUUID(h.resolveWorkspaceID(r))
	lane, ok := h.progressLane(w, r, workspaceID, false)
	if !ok {
		return
	}
	follows, err := h.Queries.ListDMProgressFollowsByLineage(r.Context(), lane.Lineage)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list follows")
		return
	}
	out := make([]dmProgressFollowResponse, 0, len(follows))
	for _, f := range follows {
		issue, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: f.IssueID, WorkspaceID: workspaceID})
		if err == nil && service.ProgressVisible(r.Context(), h.Queries, lane.Owner, issue) {
			out = append(out, dmProgressFollowToResponse(f))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"follows": out})
}

// recordIssueMoved tells every lane that follows a reparented issue where it
// went (CHE-3548). A move never edits follows or reporting roots: reporting
// stays with the original commission, and the new parent's lane hears about
// the issue only through an explicit follow. A detach has no new parent to
// name, so it writes nothing. It runs on the update's own transaction: the
// event commits or rolls back with the move, so a lane cannot miss a move.
func recordIssueMoved(ctx context.Context, q *db.Queries, prevParent pgtype.UUID, moved db.Issue) error {
	if !moved.ParentIssueID.Valid || prevParent == moved.ParentIssueID {
		return nil
	}
	return q.InsertDMProgressMovedUnderEvents(ctx, db.InsertDMProgressMovedUnderEventsParams{
		IssueID: moved.ID, WorkspaceID: moved.WorkspaceID, NewParentID: moved.ParentIssueID,
	})
}
