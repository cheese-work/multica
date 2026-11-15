package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A5b: write routes outside /api/issues that still change an issue. Each one
// names its issue through a row (comment, attachment, task, decision request)
// or a body/form field; indirectWriteTargets turns that into the issue
// references the A5a check already understands. A row that cannot be loaded
// fails closed.

// indirectWriteTargets lists the issues a write to a non-/api/issues route
// changes. Routes it does not know touch no issue and return no references.
func (h *Handler) indirectWriteTargets(w http.ResponseWriter, r *http.Request, segs []string) (refs []string, bad bool) {
	n := len(segs)
	sub := func(i int) string {
		if i < n {
			return segs[i]
		}
		return ""
	}
	switch {
	case segs[1] == "comments" && n >= 3: // /api/comments/{id}[/...]
		return h.refsOfComment(r, segs[2])
	case segs[1] == "attachments" && n == 3: // DELETE /api/attachments/{id}
		return h.refsOfAttachment(r, segs[2])
	case segs[1] == "tasks" && n == 4 && (sub(3) == "cancel" || sub(3) == "rerun"):
		return h.refsOfTask(r, segs[2])
	case segs[1] == "decisions" && n == 2: // POST /api/decisions
		return h.refsOfDecisionBody(r)
	case segs[1] == "decisions" && n == 4 && sub(3) == "withdraw":
		return h.refsOfDecisionRequest(r, segs[2])
	case segs[1] == "upload-file" && n == 2:
		return h.refsOfUploadForm(w, r)
	case segs[1] == "plugin-bridge":
		// A browser-session surface: the relay resolves the issue in the
		// workspace of X-Multica-Plugin-Installation, which need not be the
		// token's, and hooks and storage name no issue. A run token is refused.
		return nil, true
	}
	return nil, false
}

func writeScopeWorkspace(r *http.Request) (pgtype.UUID, bool) {
	id, err := util.ParseUUID(r.Header.Get("X-Workspace-ID"))
	return id, err == nil
}

func (h *Handler) refsOfComment(r *http.Request, commentRef string) (refs []string, bad bool) {
	ws, ok := writeScopeWorkspace(r)
	commentID, err := util.ParseUUID(commentRef)
	if !ok || err != nil {
		return nil, true
	}
	comment, err := h.Queries.GetCommentInWorkspace(r.Context(), db.GetCommentInWorkspaceParams{ID: commentID, WorkspaceID: ws})
	if err != nil {
		return nil, true
	}
	return []string{util.UUIDToString(comment.IssueID)}, false
}

// refsOfAttachment: an attachment on an issue or comment is a write to that
// issue. One that is not attached yet is the uploader's own to delete.
func (h *Handler) refsOfAttachment(r *http.Request, attachmentRef string) (refs []string, bad bool) {
	ws, ok := writeScopeWorkspace(r)
	attachmentID, err := util.ParseUUID(attachmentRef)
	if !ok || err != nil {
		return nil, true
	}
	att, err := h.Queries.GetAttachment(r.Context(), db.GetAttachmentParams{ID: attachmentID, WorkspaceID: ws})
	if err != nil {
		return nil, true
	}
	// UploadFile accepts both owners, so an attachment may sit on an issue and
	// on a comment of another one: every owner must be in scope.
	if att.IssueID.Valid {
		refs = append(refs, util.UUIDToString(att.IssueID))
	}
	if att.CommentID.Valid {
		commentRefs, bad := h.refsOfComment(r, util.UUIDToString(att.CommentID))
		if bad {
			return nil, true
		}
		refs = append(refs, commentRefs...)
	}
	if len(refs) > 0 {
		return refs, false
	}
	own := att.UploaderType == "agent" && util.UUIDToString(att.UploaderID) == strings.ToLower(strings.TrimSpace(r.Header.Get("X-Agent-ID")))
	return nil, !own
}

// refsOfTask: cancel and rerun act on the task's issue. A task with no issue
// (chat, autopilot) is writable only when it is the run's own task.
func (h *Handler) refsOfTask(r *http.Request, taskRef string) (refs []string, bad bool) {
	ws, ok := writeScopeWorkspace(r)
	taskID, err := util.ParseUUID(taskRef)
	if !ok || err != nil {
		return nil, true
	}
	task, err := h.Queries.GetAgentTaskInWorkspace(r.Context(), db.GetAgentTaskInWorkspaceParams{ID: taskID, WorkspaceID: ws})
	if err != nil {
		return nil, true
	}
	if task.IssueID.Valid {
		return []string{util.UUIDToString(task.IssueID)}, false
	}
	return nil, util.UUIDToString(task.ID) != strings.ToLower(strings.TrimSpace(r.Header.Get("X-Task-ID")))
}

func (h *Handler) refsOfDecisionRequest(r *http.Request, requestRef string) (refs []string, bad bool) {
	ws, ok := writeScopeWorkspace(r)
	requestID, err := util.ParseUUID(requestRef)
	if !ok || err != nil {
		return nil, true
	}
	stored, err := h.Queries.GetDecisionRequest(r.Context(), db.GetDecisionRequestParams{WorkspaceID: ws, ID: requestID})
	if err != nil {
		return nil, true
	}
	return []string{util.UUIDToString(stored.IssueID)}, false
}

func (h *Handler) refsOfDecisionBody(r *http.Request) (refs []string, bad bool) {
	b, ok := readBodyOnce(r)
	if !ok {
		return nil, true
	}
	values, bad := objectValues(b, "issue_id")
	if bad {
		return nil, true
	}
	return stringValues(values)
}

// refsOfUploadForm: a file uploaded onto an issue or a comment is a write to
// that issue. chat_session_id and task_id uploads are bound by UploadFile itself.
func (h *Handler) refsOfUploadForm(w http.ResponseWriter, r *http.Request) (refs []string, bad bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		return nil, true
	}
	if issueRef := r.FormValue("issue_id"); issueRef != "" {
		refs = append(refs, issueRef)
	}
	if commentRef := r.FormValue("comment_id"); commentRef != "" {
		commentRefs, bad := h.refsOfComment(r, commentRef)
		if bad {
			return nil, true
		}
		refs = append(refs, commentRefs...)
	}
	return refs, false
}

// stringValues decodes JSON strings; null and empty strings name no issue.
func stringValues(values []json.RawMessage) (out []string, bad bool) {
	for _, raw := range values {
		var s *string
		if json.Unmarshal(raw, &s) != nil {
			return nil, true
		}
		if s != nil && *s != "" {
			out = append(out, *s)
		}
	}
	return out, false
}
