package handler

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const askGuardEditFix = "post a new comment with multica decision request ... to ask the person, or with --notify-only if it needs no answer; an edit cannot add a member mention"

const askGuardCommentFix = "multica decision request ... to ask the person, or add --notify-only to multica issue comment add if the comment needs no answer"

// askGuardAllowsComment is S2: a plain comment from an agent task that
// mentions a member must be a typed kind. The typed kinds (ask comment,
// decision intent, record, veto, --notify-only) never reach it. It writes the
// response when the comment may not be posted.
func (h *Handler) askGuardAllowsComment(w http.ResponseWriter, r *http.Request, issue db.Issue, authorType string, task *db.AgentTaskQueue, content string) bool {
	if authorType != "agent" || len(askguard.MentionedMembers(content)) == 0 {
		return true
	}
	return h.askGuardMemberMention(w, r, issue, task, "comment", content, askGuardCommentFix)
}

// askGuardAllowsCommentEdit is S2 for an edit. A plain edit has no typed kind,
// so an agent edit that adds a member link is refused like a plain comment.
// Links the comment already had never block an unrelated edit.
func (h *Handler) askGuardAllowsCommentEdit(w http.ResponseWriter, r *http.Request, issue db.Issue, actorType, oldContent, newContent string) bool {
	if actorType != "agent" || !addsMemberMention(oldContent, newContent) {
		return true
	}
	var actorTask *db.AgentTaskQueue
	if task, ok := h.taskFromRequestHeader(r); ok {
		actorTask = &task
	}
	return h.askGuardMemberMention(w, r, issue, actorTask, "comment_edit", newContent, askGuardEditFix)
}

func addsMemberMention(oldContent, newContent string) bool {
	had := map[string]bool{}
	for _, id := range askguard.MentionedMembers(oldContent) {
		had[id] = true
	}
	for _, id := range askguard.MentionedMembers(newContent) {
		if !had[id] {
			return true
		}
	}
	return false
}

func (h *Handler) askGuardMemberMention(w http.ResponseWriter, r *http.Request, issue db.Issue, task *db.AgentTaskQueue, kind, content, fix string) bool {
	op := askguard.Op{IssueID: issue.ID, Kind: kind, Payload: []byte(content), Source: content}
	if members := askguard.MentionedMembers(content); len(members) == 1 {
		op.MemberID = parseUUID(members[0])
	}
	if task != nil {
		op.TaskID = task.ID
		op.WarnOnly = task.ChatSessionID.Valid
	} else {
		// No task token: no run to recover through, so the event only warns.
		op.TaskID = pgtype.UUID{Valid: true}
		op.WarnOnly = true
	}
	return h.askGuardHit(w, r, issue.WorkspaceID, op, fix)
}
