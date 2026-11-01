package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Revision check for the elevated read proxy (CHE-2216 plan 3.4). The proxy
// hashes the exact bytes it would return and asks whether this issue may see
// them. Any error or refusal makes the proxy return a stub, never the text.

type revisionCheckRequest struct {
	ObjectKind   string     `json:"object_kind"`
	ObjectID     int64      `json:"object_id"`
	AuthorID     int64      `json:"author_id"`
	BodyHash     string     `json:"body_hash"`
	ReviewState  string     `json:"review_state"`
	EditorType   string     `json:"editor_type"`
	EditorID     int64      `json:"editor_id"`
	LastEditedAt *time.Time `json:"last_edited_at"`
}

func (req revisionCheckRequest) valid() bool {
	editorSet := req.EditorType != "" || req.EditorID != 0
	return slices.Contains([]string{"review", "review_comment", "issue_comment"}, req.ObjectKind) &&
		req.ObjectID > 0 && req.AuthorID > 0 && opInputHash.MatchString(req.BodyHash) &&
		(!editorSet || (req.EditorType != "" && req.EditorID > 0))
}

// trustedEdit is the plan 5.2 rule for a revision-backed read: an untouched
// object, or one last edited by its own author. Original authorship alone never
// qualifies, so a foreign editor falls through to the approval path.
func (req revisionCheckRequest) trustedEdit() bool {
	if req.EditorType == "" && req.EditorID == 0 {
		return req.LastEditedAt == nil
	}
	return req.EditorType == "User" && req.EditorID == req.AuthorID
}

func (req revisionCheckRequest) snapshotHash() string {
	return ghsnapshot.ApprovalSnapshot{
		ObjectKind: req.ObjectKind, ObjectID: req.ObjectID, AuthorID: req.AuthorID, BodyHash: req.BodyHash,
		ReviewState: req.ReviewState, EditorType: req.EditorType, EditorID: req.EditorID, LastEditedAt: req.LastEditedAt,
	}.ContentHash()
}

// CheckElevatedRevision (POST /api/daemon/tasks/{taskId}/elevation/revision-check)
// answers whether the read matches an admitted intake revision (CHE-2214) or an
// exact snapshot approval (CHE-2215) for the task's issue. A task that is fenced,
// tainted or not elevated is refused outright.
func (h *Handler) CheckElevatedRevision(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireDaemonTaskAccess(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	var req revisionCheckRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !req.valid() {
		writeError(w, http.StatusBadRequest, "invalid revision read")
		return
	}
	ctx := r.Context()
	te, err := h.Queries.GetTaskElevation(ctx, task.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusForbidden, "task is not elevated")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to check the revision")
		return
	case te.State != elevationElevated || len(task.TaintIds) > 0:
		writeError(w, http.StatusForbidden, "task is fenced or tainted")
		return
	}

	via := ""
	if req.trustedEdit() {
		admitted, err := h.Queries.HasAdmittedGitHubRevision(ctx, db.HasAdmittedGitHubRevisionParams{
			WorkspaceID: te.WorkspaceID, IssueID: te.IssueID, ObjectKind: req.ObjectKind, ObjectID: req.ObjectID,
			BodyHash: req.BodyHash, AuthorID: req.AuthorID, ReviewState: req.ReviewState,
		})
		if err != nil {
			slog.Warn("github revision check failed", "task_id", uuidToString(task.ID), "error", err)
			writeError(w, http.StatusInternalServerError, "failed to check the revision")
			return
		}
		if admitted {
			via = "revision"
		}
	}
	if via == "" {
		approved, err := h.Queries.HasGitHubSnapshotApproval(ctx, db.HasGitHubSnapshotApprovalParams{
			WorkspaceID: te.WorkspaceID, IssueID: te.IssueID, SnapshotHash: req.snapshotHash(),
		})
		if err != nil {
			slog.Warn("github approval check failed", "task_id", uuidToString(task.ID), "error", err)
			writeError(w, http.StatusInternalServerError, "failed to check the revision")
			return
		}
		if approved {
			via = "approval"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"admitted": via != "", "via": via})
}
