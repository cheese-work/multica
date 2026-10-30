package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// verifyAskArtifacts refuses an ask whose artifacts are not attachments of the
// source issue in this workspace, or whose stored bytes do not hash to the
// declared sha256. A superseded attachment only has to exist on the issue.
func (handler *Handler) verifyAskArtifacts(ctx context.Context, issue db.Issue, ask decisiontx.Ask) error {
	if len(ask.Artifacts) == 0 {
		return nil
	}
	if handler.Storage == nil {
		return decisionIntentRefusal{http.StatusServiceUnavailable, "attachment storage is not configured"}
	}
	for index, artifact := range ask.Artifacts {
		prefix := fmt.Sprintf("ask.artifacts[%d]", index)
		attachment, err := handler.sourceIssueAttachment(ctx, issue, artifact.AttachmentID, prefix+".attachment_id")
		if err != nil {
			return err
		}
		digest, err := handler.attachmentSHA256(ctx, attachment)
		if err != nil {
			return decisionIntentRefusal{http.StatusBadGateway, prefix + ".attachment_id file cannot be read"}
		}
		if digest != artifact.SHA256 {
			return decisionIntentRefusal{http.StatusBadRequest, prefix + ".sha256 does not match the stored file"}
		}
		if artifact.SupersedesAttachmentID != "" {
			if _, err := handler.sourceIssueAttachment(ctx, issue, artifact.SupersedesAttachmentID, prefix+".supersedes_attachment_id"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (handler *Handler) sourceIssueAttachment(ctx context.Context, issue db.Issue, id, field string) (db.Attachment, error) {
	attachment, err := handler.Queries.GetAttachment(ctx, db.GetAttachmentParams{ID: parseUUID(id), WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return attachment, decisionIntentRefusal{http.StatusBadRequest, field + " is not an attachment in this workspace"}
	}
	if err != nil {
		return attachment, err
	}
	if attachment.IssueID != issue.ID {
		return attachment, decisionIntentRefusal{http.StatusBadRequest, field + " is not attached to the source issue"}
	}
	return attachment, nil
}

func (handler *Handler) attachmentSHA256(ctx context.Context, attachment db.Attachment) (string, error) {
	reader, err := handler.Storage.GetReader(ctx, handler.Storage.KeyFromURL(attachment.Url))
	if err != nil {
		return "", err
	}
	defer reader.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type decisionArtifactResponse struct {
	decisiontx.Artifact
	// Attachment is nil when the file was deleted after the ask was sent.
	Attachment *AttachmentResponse   `json:"attachment,omitempty"`
	Diff       *decisionArtifactDiff `json:"diff,omitempty"`
}

// decisionArtifactDiff holds the changed hunks since the superseded file.
// Problem is "not_text", "too_large" or "unavailable" when no hunks can be shown.
type decisionArtifactDiff struct {
	PreviousFilename string                `json:"previous_filename"`
	Hunks            []decisiontx.DiffHunk `json:"hunks"`
	Problem          string                `json:"problem,omitempty"`
}

// ListDecisionArtifacts returns the files an ask approves, with the changed
// hunks against the revision each one supersedes. It reads the stored ask.
func (handler *Handler) ListDecisionArtifacts(writer http.ResponseWriter, request *http.Request) {
	stored, issue, ok := handler.loadDecisionForRead(writer, request)
	if !ok {
		return
	}
	var ask decisiontx.Ask
	if len(stored.AskJson) > 0 {
		if err := json.Unmarshal(stored.AskJson, &ask); err != nil {
			writeError(writer, http.StatusInternalServerError, "failed to read stored ask")
			return
		}
	}
	mode := attachmentURLModeFromRequest(request)
	artifacts := make([]decisionArtifactResponse, 0, len(ask.Artifacts))
	for _, artifact := range ask.Artifacts {
		item := decisionArtifactResponse{Artifact: artifact}
		current, err := handler.sourceIssueAttachment(request.Context(), issue, artifact.AttachmentID, "artifact")
		if err == nil {
			response := handler.attachmentToResponse(current, mode)
			item.Attachment = &response
			if artifact.SupersedesAttachmentID != "" {
				item.Diff = handler.artifactDiff(request.Context(), issue, current, artifact.SupersedesAttachmentID)
			}
		}
		artifacts = append(artifacts, item)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"artifacts": artifacts})
}

func (handler *Handler) artifactDiff(ctx context.Context, issue db.Issue, current db.Attachment, previousID string) *decisionArtifactDiff {
	previous, err := handler.sourceIssueAttachment(ctx, issue, previousID, "artifact")
	if err != nil {
		return &decisionArtifactDiff{Hunks: []decisiontx.DiffHunk{}, Problem: "unavailable"}
	}
	diff := &decisionArtifactDiff{PreviousFilename: previous.Filename, Hunks: []decisiontx.DiffHunk{}}
	if !isTextPreviewable(current.ContentType, current.Filename) || !isTextPreviewable(previous.ContentType, previous.Filename) {
		diff.Problem = "not_text"
		return diff
	}
	oldText, oldErr := handler.readPreviewText(ctx, previous)
	newText, newErr := handler.readPreviewText(ctx, current)
	if oldErr != nil || newErr != nil {
		diff.Problem = map[bool]string{true: "too_large", false: "unavailable"}[errors.Is(oldErr, errPreviewTooLarge) || errors.Is(newErr, errPreviewTooLarge)]
		return diff
	}
	hunks, err := decisiontx.Diff(oldText, newText)
	if err != nil {
		diff.Problem = "too_large"
		return diff
	}
	diff.Hunks = hunks
	return diff
}

var errPreviewTooLarge = errors.New("file too large for diff")

func (handler *Handler) readPreviewText(ctx context.Context, attachment db.Attachment) (string, error) {
	if handler.Storage == nil {
		return "", errors.New("storage not configured")
	}
	reader, err := handler.Storage.GetReader(ctx, handler.Storage.KeyFromURL(attachment.Url))
	if err != nil {
		slog.Warn("decision artifact unreadable", "attachment_id", uuidToString(attachment.ID), "error", err)
		return "", err
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, maxPreviewTextSize+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxPreviewTextSize {
		return "", errPreviewTooLarge
	}
	return string(body), nil
}
