package handler

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const askGuardEditFix = "post a new comment with multica decision request ... to ask the person, or with --notify-only if it needs no answer; an edit cannot add a member mention"

const askGuardEditAskFix = "post a new comment with multica decision request ... with observed, files and dry_run to ask the person, or with --notify-only if it needs no answer"

const askGuardCommentFix = "multica decision request ... to ask the person, or add --notify-only to multica issue comment add if the comment needs no answer"

// askGuardAllowsComment is S2: a plain comment from an agent task that
// mentions a member must be a typed kind. A comment with no member link that
// still asks a person to act on a UI path, repo file, command or placeholder
// (decisiontx.CheckCommentAsk, CHE-5753) is refused too: it has no field for
// the evidence. The typed kinds (ask comment,
// decision intent, record, veto, --notify-only) never reach it. It writes the
// response when the comment may not be posted.
func (h *Handler) askGuardAllowsComment(w http.ResponseWriter, r *http.Request, issue db.Issue, authorType string, task *db.AgentTaskQueue, parent *db.Comment, content string) bool {
	if authorType != "agent" {
		return true
	}
	if len(askguard.MentionedMembers(content)) > 0 {
		if !h.askGuardMemberMention(w, r, issue, task, "comment", content, askGuardCommentFix) {
			return false
		}
	} else if err := decisiontx.CheckCommentAsk(content); err != nil {
		if !h.askGuardMemberMention(w, r, issue, task, "comment_ask", content, "multica decision request --kind action ... with observed, files and dry_run, or add --notify-only to multica issue comment add if the comment needs no answer (comment refused: "+err.Error()+")") {
			return false
		}
	}
	return h.askGuardReversibleComment(w, r, issue, task, parent, content)
}

// askGuardAllowsCommentEdit is S2 for an edit. A plain edit has no typed kind,
// so an agent edit that adds a member link, or turns the text into an unproven
// ask (decisiontx.CheckCommentAsk, CHE-5753), is refused like a plain comment.
// Links and asks the comment already had never block an unrelated edit; a new
// or changed unverified command, file or UI step does.
func (h *Handler) askGuardAllowsCommentEdit(w http.ResponseWriter, r *http.Request, issue db.Issue, actorType, oldContent, newContent string) bool {
	if actorType != "agent" {
		return true
	}
	var actorTask *db.AgentTaskQueue
	if task, ok := h.taskFromRequestHeader(r); ok {
		actorTask = &task
	}
	if addsMemberMention(oldContent, newContent) {
		return h.askGuardMemberMention(w, r, issue, actorTask, "comment_edit", newContent, askGuardEditFix)
	}
	// An ask the comment already held stays editable, but an edit may not bring
	// in a command, file or UI step it did not cite before (CHE-6354).
	err := decisiontx.CheckCommentAsk(newContent)
	if err != nil && (decisiontx.CheckCommentAsk(oldContent) == nil || decisiontx.AddsAskItem(oldContent, newContent)) {
		return h.askGuardMemberMention(w, r, issue, actorTask, "comment_edit", newContent, askGuardEditAskFix+" (edit refused: "+err.Error()+")")
	}
	return true
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

const waitingCommentKind = "waiting_comment"

const waitingCommentFix = "multica issue comment add --ask <member-id> if a person acts next, or --no-ask if no one does; a member mention, --notify-only or multica decision request also counts"

// askGuardAllowsWaitingComment is S4 (CHE-7289): an agent comment on an issue in
// in_review, blocked or need_decision must carry a typed audience. A member
// mention, a typed kind, --ask <member-id> or --no-ask is one; so is a mention
// of an agent or squad, or a reply under an agent's comment (agent to agent).
// It reads typed fields and structural links only, never the comment text.
// The waiting_audience_guard setting decides: off allows, warn records a hit,
// block records it and refuses with 422. A chat-lane run or a call with no
// task only warns, as in S2. Set the setting to off to revert; no data changes.
func (h *Handler) askGuardAllowsWaitingComment(w http.ResponseWriter, r *http.Request, issue db.Issue, authorType string, task *db.AgentTaskQueue, parent *db.Comment, req CreateCommentRequest) bool {
	if authorType != "agent" {
		return true
	}
	if req.Ask != "" && req.NoAsk {
		writeError(w, http.StatusBadRequest, "ask and no_ask cannot combine")
		return false
	}
	if req.Ask != "" {
		askID, err := util.ParseUUID(req.Ask)
		if err == nil {
			_, err = h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: askID, WorkspaceID: issue.WorkspaceID})
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "ask must be the id of a member of this workspace")
			return false
		}
	}
	typed := req.Ask != "" || req.NoAsk || req.NotifyOnly || req.DecisionIntent || req.DecisionRecord || req.DecisionVeto
	waiting := issue.Status == "in_review" || issue.Status == "blocked" || issue.Status == "need_decision"
	if !waiting || typed || len(askguard.MentionedMembers(req.Content)) > 0 || mentionsAgentOrSquad(req.Content) || (parent != nil && parent.AuthorType == "agent") {
		return true
	}
	workspace, err := h.Queries.GetWorkspace(r.Context(), issue.WorkspaceID)
	var mode askguard.Mode
	if err == nil {
		mode, err = askguard.WaitingAudienceModeFromSettings(workspace.Settings)
	}
	if err == nil && mode != askguard.ModeOff {
		op := askguard.Op{IssueID: issue.ID, Kind: waitingCommentKind, Payload: []byte(req.Content), TaskID: pgtype.UUID{Valid: true}}
		if task != nil {
			op.TaskID = task.ID
		}
		err = askguard.Guard{Queries: h.Queries, Metrics: h.Metrics}.RecordWarn(r.Context(), issue.WorkspaceID, op)
	}
	if err != nil {
		slog.Warn("waiting audience guard failed", "error", err, "issue_id", uuidToString(issue.ID))
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "ask guard unavailable; retry the same request")
		return false
	}
	if mode == askguard.ModeBlock && task != nil && !task.ChatSessionID.Valid {
		writeError(w, http.StatusUnprocessableEntity, "this comment on a "+issue.Status+" issue names no audience; run: "+waitingCommentFix)
		return false
	}
	return true
}
