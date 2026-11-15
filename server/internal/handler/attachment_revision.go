package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// chainAttachmentRevision makes an issue-level upload the next revision of an
// earlier file (CHE-5295): the file named by `replaces`, or else the newest
// issue file with the same name. It returns a non-zero HTTP status to refuse
// the upload. Comment, chat and task uploads never chain.
func (h *Handler) chainAttachmentRevision(ctx context.Context, params *db.CreateAttachmentParams, replaces string, data []byte) (int, string) {
	sum := sha256.Sum256(data)
	params.Sha256 = pgtype.Text{String: hex.EncodeToString(sum[:]), Valid: true}

	issueLevel := params.IssueID.Valid && !params.CommentID.Valid && !params.ChatSessionID.Valid && !params.TaskID.Valid
	if !issueLevel {
		if replaces != "" {
			return http.StatusBadRequest, "replaces needs an issue-level upload"
		}
		return 0, ""
	}

	var prev db.Attachment
	var err error
	if replaces != "" {
		prevID, perr := parseUUIDStrict(replaces)
		if perr != nil {
			return http.StatusBadRequest, "replaces must be an attachment id"
		}
		prev, err = h.Queries.GetAttachment(ctx, db.GetAttachmentParams{ID: prevID, WorkspaceID: params.WorkspaceID})
		if err != nil || prev.IssueID != params.IssueID || prev.CommentID.Valid || prev.ChatSessionID.Valid || prev.TaskID.Valid || prev.SourceContextID.Valid {
			return http.StatusBadRequest, "replaces must name a file on the same issue"
		}
	} else {
		prev, err = h.Queries.GetLatestIssueAttachmentByName(ctx, db.GetLatestIssueAttachmentByNameParams{
			WorkspaceID: params.WorkspaceID,
			IssueID:     params.IssueID,
			Filename:    params.Filename,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ""
		}
		if err != nil {
			slog.Error("attachment revision lookup failed", "error", err)
			return http.StatusInternalServerError, "upload failed"
		}
	}

	chain, err := h.Queries.ListAttachmentChain(ctx, db.ListAttachmentChainParams{WorkspaceID: params.WorkspaceID, ChainID: chainID(prev)})
	if err != nil || len(chain) == 0 {
		slog.Error("attachment chain lookup failed", "error", err)
		return http.StatusInternalServerError, "upload failed"
	}
	// Always continue from the head, so replacing an old revision never forks.
	head := chain[len(chain)-1]
	params.Revision = pgtype.Int4{Int32: head.Revision + 1, Valid: true}
	params.RevisionOf = head.ID
	// chain[0] is revision 1 only while revision 1 still exists; the chain id
	// of any member is stable, so a deleted first revision never splits it.
	params.RevisionRoot = chainID(prev)
	return 0, ""
}

func chainID(a db.Attachment) pgtype.UUID {
	if a.RevisionRoot.Valid {
		return a.RevisionRoot
	}
	return a.ID
}

type attachmentRevisionResponse struct {
	ID           string  `json:"id"`
	Revision     int     `json:"revision"`
	RevisionOf   *string `json:"revision_of"`
	UploaderType string  `json:"uploader_type"`
	UploaderID   string  `json:"uploader_id"`
	SHA256       string  `json:"sha256"`
	SizeBytes    int64   `json:"size_bytes"`
	CreatedAt    string  `json:"created_at"`
}

// ListAttachmentRevisions — GET /api/attachments/{id}/history
//
// Every revision of the document the attachment belongs to, oldest first.
func (h *Handler) ListAttachmentRevisions(w http.ResponseWriter, r *http.Request) {
	att, ok := h.loadAttachmentForRequest(w, r)
	if !ok {
		return
	}
	chain, err := h.Queries.ListAttachmentChain(r.Context(), db.ListAttachmentChainParams{WorkspaceID: att.WorkspaceID, ChainID: chainID(att)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load attachment history")
		return
	}
	out := make([]attachmentRevisionResponse, 0, len(chain))
	for _, a := range chain {
		item := attachmentRevisionResponse{
			ID:           uuidToString(a.ID),
			Revision:     int(a.Revision),
			UploaderType: a.UploaderType,
			UploaderID:   uuidToString(a.UploaderID),
			SHA256:       a.Sha256,
			SizeBytes:    a.SizeBytes,
			CreatedAt:    a.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		}
		if a.RevisionOf.Valid {
			s := uuidToString(a.RevisionOf)
			item.RevisionOf = &s
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
}

// DiffAttachmentRevisions — GET /api/attachments/{id}/diff?from=N&to=M
//
// Unified diff for text files; binary files report only that they differ.
func (h *Handler) DiffAttachmentRevisions(w http.ResponseWriter, r *http.Request) {
	att, ok := h.loadAttachmentForRequest(w, r)
	if !ok {
		return
	}
	from, ok := parseRevisionParam(w, r, "from")
	if !ok {
		return
	}
	to, ok := parseRevisionParam(w, r, "to")
	if !ok {
		return
	}
	chain, err := h.Queries.ListAttachmentChain(r.Context(), db.ListAttachmentChainParams{WorkspaceID: att.WorkspaceID, ChainID: chainID(att)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load attachment history")
		return
	}
	var a, b *db.Attachment
	for i := range chain {
		switch int64(chain[i].Revision) {
		case from:
			a = &chain[i]
		case to:
			b = &chain[i]
		}
	}
	if from == to && a != nil {
		b = a
	}
	if a == nil || b == nil {
		writeError(w, http.StatusNotFound, "revision not found")
		return
	}
	if h.denyWithheldAttachment(w, r, *a) || h.denyWithheldAttachment(w, r, *b) {
		return
	}

	resp := map[string]any{"from": from, "to": to}
	if !isTextPreviewable(a.ContentType, a.Filename) || !isTextPreviewable(b.ContentType, b.Filename) {
		resp["binary"] = true
		if a.SizeBytes != b.SizeBytes || a.Sha256 != b.Sha256 {
			resp["message"] = "binary, sizes and SHA differ"
		} else {
			resp["message"] = "binary, identical"
		}
		resp["from_size_bytes"], resp["to_size_bytes"] = a.SizeBytes, b.SizeBytes
		resp["from_sha256"], resp["to_sha256"] = a.Sha256, b.Sha256
		writeJSON(w, http.StatusOK, resp)
		return
	}

	oldText, status, msg := h.readAttachmentText(r.Context(), *a)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	newText, status, msg := h.readAttachmentText(r.Context(), *b)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	ctxLines := 3
	if r.URL.Query().Get("full") == "1" {
		ctxLines = 1 << 24 // whole file, for the "Show full" toggle
	}
	diff, err := unifiedDiffContext(oldText, newText, fmt.Sprintf("v%d", from), fmt.Sprintf("v%d", to), ctxLines)
	if errors.Is(err, errDiffTooLarge) {
		writeError(w, http.StatusUnprocessableEntity, "file too large to diff")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to diff")
		return
	}
	resp["binary"] = false
	resp["diff"] = diff
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) readAttachmentText(ctx context.Context, a db.Attachment) (string, int, string) {
	if h.Storage == nil {
		return "", http.StatusServiceUnavailable, "storage not configured"
	}
	reader, err := h.Storage.GetReader(ctx, h.Storage.KeyFromURL(a.Url))
	if err != nil {
		slog.Error("failed to open attachment revision", "id", uuidToString(a.ID), "error", err)
		return "", http.StatusNotFound, "attachment object not found"
	}
	defer reader.Close()
	limit := h.maxPreviewSizeBytes()
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return "", http.StatusBadGateway, "failed to read attachment body"
	}
	if int64(len(body)) > limit {
		return "", http.StatusRequestEntityTooLarge, "file too large to diff"
	}
	return string(body), 0, ""
}
