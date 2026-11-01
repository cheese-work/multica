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

// ApproveReadSnapshot approves one object's body hash for one issue. Human
// owner/admin only, like a grant: any agent credential gets 403.
func (h *Handler) ApproveReadSnapshot(w http.ResponseWriter, r *http.Request) {
	issue, member, ok := h.requireHumanIssueAdmin(w, r, "only a person can approve a read")
	if !ok {
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
