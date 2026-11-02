package handler

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const commentPreviewMaxRunes = 140

// markdownLinkRe reduces `[label](url)` to `label` so a mention in the first
// line previews as its name, not as a URL.
var markdownLinkRe = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

// CommentPreviewResponse is what a rendered comment-ID chip needs: where the
// comment lives and enough text to recognise it.
type CommentPreviewResponse struct {
	CommentID       string `json:"comment_id"`
	IssueID         string `json:"issue_id"`
	IssueIdentifier string `json:"issue_identifier"`
	AuthorType      string `json:"author_type"`
	AuthorID        string `json:"author_id"`
	AuthorName      string `json:"author_name"`
	Preview         string `json:"preview"`
}

func commentFirstLine(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(markdownLinkRe.ReplaceAllString(line, "$1"))
		line = strings.TrimLeft(line, "#> ")
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > commentPreviewMaxRunes {
			return string(r[:commentPreviewMaxRunes]) + "…"
		}
		return line
	}
	return ""
}

// GetCommentPreview resolves a comment UUID to its issue and a first-line
// preview. A comment outside the caller's workspace, or deleted, is a plain 404
// so the response never reveals whether the id exists.
func (h *Handler) GetCommentPreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireUserID(w, r); !ok {
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	commentUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "commentId"), "comment id")
	if !ok {
		return
	}
	comment, err := h.Queries.GetCommentInWorkspace(r.Context(), db.GetCommentInWorkspaceParams{
		ID:          commentUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil || comment.DeletedAt.Valid {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}
	issue, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
		ID:          comment.IssueID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}
	prefix := h.getIssuePrefix(r.Context(), wsUUID)
	writeJSON(w, http.StatusOK, CommentPreviewResponse{
		CommentID:       uuidToString(comment.ID),
		IssueID:         uuidToString(issue.ID),
		IssueIdentifier: issueToResponse(issue, prefix).Identifier,
		AuthorType:      comment.AuthorType,
		AuthorID:        uuidToString(comment.AuthorID),
		AuthorName:      h.actorDisplayName(r.Context(), wsUUID, comment.AuthorType, comment.AuthorID),
		Preview:         commentFirstLine(comment.Content),
	})
}
