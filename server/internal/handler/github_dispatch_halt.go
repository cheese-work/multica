package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// githubHaltScopeRe is '*', a GitHub owner, or owner/repo, lower-cased.
var githubHaltScopeRe = regexp.MustCompile(`^(\*|[a-z0-9._-]+(/[a-z0-9._-]+)?)$`)

type githubDispatchHaltResponse struct {
	Scope    string  `json:"scope"`
	HaltedBy *string `json:"halted_by"`
	HaltedAt string  `json:"halted_at"`
}

func (h *Handler) listGitHubDispatchHalts(r *http.Request, workspaceID string) ([]githubDispatchHaltResponse, error) {
	rows, err := h.Queries.ListGitHubDispatchHalts(r.Context(), parseUUID(workspaceID))
	if err != nil {
		return nil, err
	}
	out := make([]githubDispatchHaltResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, githubDispatchHaltResponse{Scope: row.Scope, HaltedBy: uuidToPtr(row.HaltedBy), HaltedAt: timestampToString(row.HaltedAt)})
	}
	return out, nil
}

// GetGitHubDispatchHalt (GET /api/github/dispatch-halt) reads the kill switch
// without touching it.
func (h *Handler) GetGitHubDispatchHalt(w http.ResponseWriter, r *http.Request) {
	halts, err := h.listGitHubDispatchHalts(r, h.resolveWorkspaceID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read dispatch halts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"halts": halts})
}

// SetGitHubDispatchHalt (PUT /api/github/dispatch-halt) turns the kill switch
// on or off for a scope. Turning it on also cancels the live runs GitHub
// started in that scope and revokes their tokens. A machine credential may
// only turn it on: an agent must not be able to lift a halt.
func (h *Handler) SetGitHubDispatchHalt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope  string `json:"scope"`
		Halted *bool  `json:"halted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Halted == nil {
		writeError(w, http.StatusBadRequest, "scope and halted are required")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if !githubHaltScopeRe.MatchString(scope) {
		writeError(w, http.StatusBadRequest, "scope must be *, an owner, or owner/repo")
		return
	}
	if !*req.Halted && isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "only a human can turn the kill switch off")
		return
	}
	ctx := r.Context()
	workspaceID := parseUUID(h.resolveWorkspaceID(r))
	userID := parseUUID(requestUserID(r))
	revoked := 0
	if *req.Halted {
		if err := h.Queries.HaltGitHubDispatch(ctx, db.HaltGitHubDispatchParams{WorkspaceID: workspaceID, Scope: scope, HaltedBy: userID}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to turn the kill switch on")
			return
		}
		// The halt row is committed first, so nothing new starts while we sweep.
		ids, err := h.Queries.ListGitHubTriggeredActiveTasks(ctx, db.ListGitHubTriggeredActiveTasksParams{WorkspaceID: workspaceID, Scope: scope})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "kill switch is on, but listing live GitHub-triggered runs failed")
			return
		}
		for _, id := range ids {
			if _, err := h.TaskService.CancelTaskWithReason(ctx, id, "cancelled by GitHub dispatch kill switch", "github_dispatch_halted"); err != nil {
				slog.Warn("github dispatch halt: cancel run failed", "task_id", uuidToString(id), "err", err)
			}
			if err := h.Queries.DeleteTaskTokensByTask(ctx, id); err != nil {
				writeError(w, http.StatusInternalServerError, "kill switch is on, but revoking a run token failed")
				return
			}
			revoked++
		}
	} else if _, err := h.Queries.ResumeGitHubDispatch(ctx, db.ResumeGitHubDispatchParams{WorkspaceID: workspaceID, Scope: scope}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to turn the kill switch off")
		return
	}
	slog.Warn("github dispatch halt changed", "workspace_id", uuidToString(workspaceID), "scope", scope, "halted", *req.Halted, "actor", requestUserID(r), "runs_revoked", revoked)
	halts, err := h.listGitHubDispatchHalts(r, uuidToString(workspaceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read dispatch halts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"halts": halts, "runs_revoked": revoked})
}
