package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Scheduling pause (CHE-2954): a person who owns or administers the workspace
// stops or restarts claims for one agent or the whole workspace. Enqueue and
// running tasks are untouched; the claim fence and the resume cache bump live in
// the queries and TaskService. Audit: workspace_audit_event is not on main yet
// (PR #713), so the actor is recorded only in scheduling_paused_by.

// requireSchedulingAdmin admits a person who owns or administers workspaceID.
// Any agent credential, including the task token of an agent a member owns, gets
// 403 before the workspace role is read: canManageAgent would let that member-owner act.
func (h *Handler) requireSchedulingAdmin(w http.ResponseWriter, r *http.Request, workspaceID string) (string, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return "", false
	}
	if actorType, _ := h.resolveActor(r, userID, workspaceID); isGovernedFieldMachineActor(r, actorType) {
		writeError(w, http.StatusForbidden, "only a person can pause or resume scheduling")
		return "", false
	}
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return "", false
	}
	return userID, true
}

func (h *Handler) PauseAgentScheduling(w http.ResponseWriter, r *http.Request) {
	h.setAgentScheduling(w, r, true)
}

func (h *Handler) ResumeAgentScheduling(w http.ResponseWriter, r *http.Request) {
	h.setAgentScheduling(w, r, false)
}

func (h *Handler) setAgentScheduling(w http.ResponseWriter, r *http.Request, pause bool) {
	wsID := h.resolveWorkspaceID(r)
	if wsID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	userID, ok := h.requireSchedulingAdmin(w, r, wsID)
	if !ok {
		return
	}
	agent, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}

	var (
		updated db.Agent
		err     error
	)
	if pause {
		updated, err = h.TaskService.PauseAgentScheduling(r.Context(), agent.WorkspaceID, agent.ID, parseUUID(userID))
	} else {
		updated, err = h.TaskService.ResumeAgentScheduling(r.Context(), agent.WorkspaceID, agent.ID)
	}
	if err != nil {
		slog.Warn("set agent scheduling failed", append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID), "pause", pause)...)
		writeError(w, http.StatusInternalServerError, "failed to update agent scheduling")
		return
	}

	resp := h.agentToResponse(updated)
	if err := h.attachAgentSkills(r.Context(), &resp, updated.ID); err != nil {
		slog.Warn("load agent skills after scheduling change failed", append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to load agent skills")
		return
	}
	// A no-op (already paused / already running) answers 200 with the current
	// state but publishes nothing.
	actorType, actorID := h.resolveActor(r, userID, wsID)
	if agent.SchedulingPausedAt.Valid != pause {
		eventType := protocol.EventAgentSchedulingResumed
		if pause {
			eventType = protocol.EventAgentSchedulingPaused
		}
		h.publish(eventType, wsID, actorType, actorID, map[string]any{"agent": broadcastAgentResponse(resp)})
	}
	redactAgentResponseForActor(&resp, actorType)
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) PauseWorkspaceScheduling(w http.ResponseWriter, r *http.Request) {
	h.setWorkspaceScheduling(w, r, true)
}

func (h *Handler) ResumeWorkspaceScheduling(w http.ResponseWriter, r *http.Request) {
	h.setWorkspaceScheduling(w, r, false)
}

func (h *Handler) setWorkspaceScheduling(w http.ResponseWriter, r *http.Request, pause bool) {
	wsID := workspaceIDFromURL(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	userID, ok := h.requireSchedulingAdmin(w, r, wsID)
	if !ok {
		return
	}
	ws, err := h.Queries.GetWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}

	var updated db.Workspace
	if pause {
		updated, err = h.TaskService.PauseWorkspaceScheduling(r.Context(), wsUUID, parseUUID(userID))
	} else {
		updated, err = h.TaskService.ResumeWorkspaceScheduling(r.Context(), wsUUID)
	}
	if err != nil {
		slog.Warn("set workspace scheduling failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", wsID, "pause", pause)...)
		writeError(w, http.StatusInternalServerError, "failed to update workspace scheduling")
		return
	}

	resp := h.workspaceToResponse(updated)
	if ws.SchedulingPausedAt.Valid != pause {
		eventType := protocol.EventWorkspaceSchedulingResumed
		if pause {
			eventType = protocol.EventWorkspaceSchedulingPaused
		}
		actorType, actorID := h.resolveActor(r, userID, wsID)
		h.publish(eventType, wsID, actorType, actorID, map[string]any{"workspace": resp})
	}
	writeJSON(w, http.StatusOK, resp)
}
