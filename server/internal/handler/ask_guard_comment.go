package handler

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const askGuardCommentFix = "multica decision request ... to ask the person, or add --notify-only to multica issue comment add if the comment needs no answer"

// askGuardAllowsComment is S2: a plain comment from an agent task that
// mentions a member must be a typed kind. The typed kinds (ask comment,
// decision intent, record, veto, --notify-only) never reach it. It writes the
// response when the comment may not be posted.
func (h *Handler) askGuardAllowsComment(w http.ResponseWriter, r *http.Request, issue db.Issue, authorType string, task *db.AgentTaskQueue, content string) bool {
	if authorType != "agent" || len(askguard.MentionedMembers(content)) == 0 {
		return true
	}
	op := askguard.Op{IssueID: issue.ID, Kind: "comment", Payload: []byte(content)}
	if task != nil {
		op.TaskID = task.ID
		op.WarnOnly = task.ChatSessionID.Valid
	} else {
		// No task token: no run to recover through, so the event only warns.
		op.TaskID = pgtype.UUID{Valid: true}
		op.WarnOnly = true
	}
	return h.askGuardHit(w, r, issue.WorkspaceID, op, askGuardCommentFix)
}
