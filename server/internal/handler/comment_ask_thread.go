package handler

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Ask threads (T5a, CHE-6429): a comment created with `from_thread` is a new
// root whose ask_source_root_id is the thread root of the named comment. The
// server writes one system stub in the source thread in the same transaction.

// askSourceRootForFromThread resolves req.FromThread to the source thread root.
// It writes the refusal itself and reports ok=false. A request without
// from_thread returns an invalid UUID.
func (h *Handler) askSourceRootForFromThread(w http.ResponseWriter, r *http.Request, issue db.Issue, req CreateCommentRequest) (pgtype.UUID, bool) {
	if req.FromThread == "" {
		return pgtype.UUID{}, true
	}
	if req.ParentID != nil {
		writeError(w, http.StatusBadRequest, "from_thread opens a new root and cannot combine with parent_id")
		return pgtype.UUID{}, false
	}
	sourceID, ok := parseUUIDOrBadRequest(w, req.FromThread, "from_thread")
	if !ok {
		return pgtype.UUID{}, false
	}
	source, err := h.Queries.GetComment(r.Context(), sourceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "from_thread names no comment")
		return pgtype.UUID{}, false
	}
	if uuidToString(source.IssueID) != uuidToString(issue.ID) {
		writeErrorCode(w, http.StatusBadRequest, "thread_issue_mismatch", "from_thread must name a comment on this issue")
		return pgtype.UUID{}, false
	}
	root, err := h.Queries.GetThreadRoot(r.Context(), db.GetThreadRootParams{CommentID: source.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusBadRequest, "from_thread names no thread")
		return pgtype.UUID{}, false
	}
	return root.ID, true
}

// askSourceStubParams builds the system reply in the source thread. It carries
// plain names, never mention links, so it triggers nothing.
func askSourceStubParams(issue db.Issue, sourceRootID, askRootID pgtype.UUID, askContent string) db.CreateCommentParams {
	text := "Asked"
	if names := mentionedNames(askContent); len(names) > 0 {
		text += " " + strings.Join(names, ", ")
	}
	text += fmt.Sprintf(" in thread %s.", uuidToString(askRootID))
	return db.CreateCommentParams{
		ID:          dbid.NewV7(),
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     text,
		Type:        "comment",
		ParentID:    sourceRootID,
	}
}

// mentionedNames lists the labels of the members, agents and squads a comment
// mentions, in order of first appearance. Brackets are dropped so a label can
// never rebuild a mention link inside the stub.
func mentionedNames(content string) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range util.MentionRe.FindAllStringSubmatch(content, -1) {
		if m[2] == "issue" || m[2] == "all" {
			continue
		}
		name := strings.Join(strings.Fields(strings.NewReplacer("[", "", "]", "").Replace(m[1])), " ")
		if len(name) > 80 {
			name = name[:80]
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// askRootNotResolvableMessage is the 409 text an agent task gets when it tries
// to resolve the root of an ask thread.
func askRootNotResolvableMessage(sourceRootID pgtype.UUID) string {
	return fmt.Sprintf("This comment opens an ask thread; it cannot resolve itself. Resolve the source thread %s with a reply there.", uuidToString(sourceRootID))
}

// publishAskSourceStub announces the stub after commit, like any new comment.
func (h *Handler) publishAskSourceStub(issue db.Issue, stub db.CreateCommentRow) {
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", uuidToString(stub.AuthorID), map[string]any{
		"comment":             commentToResponse(stub.Comment(), nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      stub.IssueRevision,
	})
}
