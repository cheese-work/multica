package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Per-issue workflow override (CHE-1986). An issue follows its own workflow
// when issue.workflow_id is set, otherwise its project's. issueworkflow.ForIssue
// is the one resolver.

// SetIssueWorkflowRequest sets or clears an issue's workflow override.
type SetIssueWorkflowRequest struct {
	// WorkflowID is the workflow the issue follows; null returns it to its
	// project's workflow.
	WorkflowID *string `json:"workflow_id"`
}

// requireIssueWorkflowManager admits workspace owners and admins, and the lead
// of the issue's project (the zero project outside any), like a project's own
// workflow settings.
func (h *Handler) requireIssueWorkflowManager(w http.ResponseWriter, r *http.Request, workspaceID string, project db.Project) (db.Member, bool) {
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return db.Member{}, false
	}
	if !canManageProjectWorkflow(member, project) {
		writeError(w, http.StatusForbidden, "only workspace admins and the project lead can set an issue's workflow")
		return db.Member{}, false
	}
	return member, true
}

// SetIssueWorkflow sets or clears an issue's workflow override. The switch is
// administrative: no handoff, no run, the assignee stays. It is refused when
// the issue's current status is not in the workflow it would follow, and while
// the issue has a live task, because a deferred retry keeps its old workflow
// step and would pass the stale-step check under the new workflow.
func (h *Handler) SetIssueWorkflow(w http.ResponseWriter, r *http.Request) {
	loaded, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req SetIssueWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var target pgtype.UUID
	if req.WorkflowID != nil && *req.WorkflowID != "" {
		if !h.requireProjectWorkflowsV1(w, r) {
			return
		}
		id, ok := parseUUIDOrBadRequest(w, *req.WorkflowID, "workflow_id")
		if !ok {
			return
		}
		target = id
	}
	workspaceID := uuidToString(loaded.WorkspaceID)
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{ID: loaded.ProjectID, WorkspaceID: loaded.WorkspaceID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to set workflow")
		return
	}
	member, ok := h.requireIssueWorkflowManager(w, r, workspaceID, project)
	if !ok {
		return
	}

	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set workflow")
		return
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	// The lock order every issue writer takes: catalog (shared, so a workflow
	// edit cannot change the target's steps under this check), project, issue.
	if err := qtx.LockIssueStatusCatalogShared(ctx, loaded.WorkspaceID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set workflow")
		return
	}
	if loaded.ProjectID.Valid {
		if _, err := qtx.LockProjectForIssueWrite(ctx, db.LockProjectForIssueWriteParams{ID: loaded.ProjectID, WorkspaceID: loaded.WorkspaceID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "failed to set workflow")
			return
		}
	}
	issue, err := qtx.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: loaded.ID, WorkspaceID: loaded.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	if issue.ProjectID != loaded.ProjectID {
		writeJSON(w, http.StatusConflict, map[string]any{"error": errWorkflowChanged.Error() + "; retry", "code": "workflow_changed"})
		return
	}
	if issue.WorkflowID == target {
		writeJSON(w, http.StatusOK, h.issueResponseWithCategory(ctx, issue))
		return
	}
	// The workflow the issue would follow, whether the override or, once
	// cleared, the project's.
	next, _, err := issueworkflow.ForIssue(ctx, qtx, issue.WorkspaceID, target, issue.ProjectID)
	if writeIssueWorkflowError(w, err) {
		return
	}
	if err != nil {
		slog.Warn("set issue workflow: resolve workflow", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to set workflow")
		return
	}
	if err := issueworkflow.CheckStatus(next, issue.Status); err != nil {
		writeIssueWorkflowError(w, err)
		return
	}
	live, err := qtx.HasLiveTaskForIssue(ctx, issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set workflow")
		return
	}
	if live {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "the issue has a task that can still run; wait for it to finish or cancel it, then change the workflow",
			"code":  "issue_has_live_task",
		})
		return
	}
	updated, err := qtx.SetIssueWorkflow(ctx, db.SetIssueWorkflowParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, WorkflowID: target})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set workflow")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set workflow")
		return
	}
	h.publishIssueSnapshots(ctx, []db.Issue{updated}, "member", uuidToString(member.UserID))
	writeJSON(w, http.StatusOK, h.issueResponseWithCategory(ctx, updated))
}

func (h *Handler) issueResponseWithCategory(ctx context.Context, issue db.Issue) IssueResponse {
	resp := issueToResponse(issue, h.getIssuePrefix(ctx, issue.WorkspaceID))
	h.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	return resp
}
