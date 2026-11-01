package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// read_snapshot_approval (CHE-2216 plan 3.4 and 3.7, slice B3): a person approves
// the exact bytes of one object for one issue, so an elevated run reads it without
// a downgrade. It covers objects with no admission path and Multica bodies written
// by a run that read external text. The read proxy (B2) and the Multica read
// filter (B8) call hasReadSnapshotApproval; this slice only stores and looks up.
var (
	readApprovalKinds  = []string{"pr_title", "pr_body", "commit_message", "ci_log", "multica_comment", "multica_description"}
	readApprovalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type readSnapshotApprovalRequest struct {
	ObjectKind string `json:"object_kind"`
	ObjectID   string `json:"object_id"`
	BodyHash   string `json:"body_hash"`
}

// requireCanRouteIssue writes 403 unless the person passes can_route (plan 3.4)
// for the agent the issue is assigned to; 409 when no agent is assigned, 503 when a lookup fails.
func (h *Handler) requireCanRouteIssue(w http.ResponseWriter, r *http.Request, issue db.Issue, userID pgtype.UUID) bool {
	if !issue.AssigneeType.Valid || issue.AssigneeType.String != "agent" || !issue.AssigneeID.Valid {
		writeError(w, http.StatusConflict, "the issue has no agent assignee to route to")
		return false
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusConflict, "the issue's agent assignee was not found")
		return false
	}
	switch h.canRouteMember(r.Context(), userID, issue, agent).Verdict {
	case routeAllowed:
		return true
	case routeDeferred:
		writeError(w, http.StatusServiceUnavailable, "could not check route permission; retry")
	default:
		writeError(w, http.StatusForbidden, "you may not route work on this issue")
	}
	return false
}

// ApproveReadSnapshot approves one object's body hash for one issue. A person
// who passes can_route for the issue's agent only: any agent credential gets 403.
func (h *Handler) ApproveReadSnapshot(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.requireHumanIssue(w, r, "only a person can approve a read")
	if !ok {
		return
	}
	member, ok := h.requireWorkspaceMember(w, r, uuidToString(issue.WorkspaceID), "workspace not found")
	if !ok || !h.requireCanRouteIssue(w, r, issue, member.UserID) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req readSnapshotApprovalRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !slices.Contains(readApprovalKinds, req.ObjectKind) || req.ObjectID == "" || len(req.ObjectID) > 256 || !readApprovalSHA256.MatchString(req.BodyHash) {
		writeError(w, http.StatusBadRequest, "object_kind, object_id and a lowercase SHA-256 body_hash are required")
		return
	}
	if _, err := h.Queries.InsertReadSnapshotApproval(r.Context(), db.InsertReadSnapshotApprovalParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ObjectKind: req.ObjectKind, ObjectID: req.ObjectID,
		BodyHash: req.BodyHash, ApproverUserID: member.UserID,
	}); err != nil {
		slog.Warn("approve read snapshot failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to record the approval")
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

// hasReadSnapshotApproval is true only for an approval of these exact bytes for this issue.
// Callers treat an error as "not approved" and return a stub.
func (h *Handler) hasReadSnapshotApproval(ctx context.Context, workspaceID, issueID pgtype.UUID, kind, objectID, bodyHash string) (bool, error) {
	return h.Queries.HasReadSnapshotApproval(ctx, db.HasReadSnapshotApprovalParams{
		WorkspaceID: workspaceID, IssueID: issueID, ObjectKind: kind, ObjectID: objectID, BodyHash: bodyHash,
	})
}
