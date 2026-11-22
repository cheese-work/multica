package handler

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

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

const waitingMoveKind = "waiting_move"

const waitingMoveFix = "multica issue status <id> <status> --waiting-on member:<member-id> if a person acts next, or --waiting-on none if no one does; a decision request or member mention in this task also counts"

func isWaitingStatus(status string) bool {
	return status == "in_review" || status == "blocked" || status == "need_decision"
}

// askGuardAllowsWaitingMove is S5 (CHE-7290): an agent that moves an issue into
// in_review, blocked or need_decision must say who acts next with waiting_on,
// "member:<user id>" or "none". A task that already made a typed ask, a
// decision request or a comment with a member mention on this issue, is
// exempt. It reads typed fields and structural links only, never the text of
// earlier comments, so a plain ask posted while the issue was in progress is
// judged at the move. The waiting_on_guard setting decides: off allows, warn
// records a hit, block records it and refuses with 422. A chat-lane run or a
// call with no task only warns, as in S4. Set the setting to off to revert.
func (h *Handler) askGuardAllowsWaitingMove(w http.ResponseWriter, r *http.Request, issue db.Issue, actorType, newStatus string, waitingOn *string) bool {
	if actorType != "agent" {
		return true
	}
	if waitingOn != nil && !h.validWaitingOn(w, r, issue, *waitingOn) {
		return false
	}
	if newStatus == "" || newStatus == issue.Status || !isWaitingStatus(newStatus) || waitingOn != nil {
		return true
	}
	var task *db.AgentTaskQueue
	if t, ok := h.taskFromRequestHeader(r); ok {
		task = &t
	}
	if task != nil && h.taskMadeTypedAsk(r, issue, *task) {
		return true
	}
	workspace, err := h.Queries.GetWorkspace(r.Context(), issue.WorkspaceID)
	var mode askguard.Mode
	if err == nil {
		mode, err = askguard.WaitingOnModeFromSettings(workspace.Settings)
	}
	if err == nil && mode != askguard.ModeOff {
		op := askguard.Op{IssueID: issue.ID, Kind: waitingMoveKind, Payload: []byte(issue.Status + "->" + newStatus), TaskID: pgtype.UUID{Valid: true}}
		if task != nil {
			op.TaskID = task.ID
		}
		err = askguard.Guard{Queries: h.Queries, Metrics: h.Metrics}.RecordWarn(r.Context(), issue.WorkspaceID, op)
	}
	if err != nil {
		slog.Warn("waiting on guard failed", "error", err, "issue_id", uuidToString(issue.ID))
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "ask guard unavailable; retry the same request")
		return false
	}
	if mode == askguard.ModeBlock && task != nil && !task.ChatSessionID.Valid {
		writeError(w, http.StatusUnprocessableEntity, "moving this issue to "+newStatus+" names no one to act next; run: "+waitingMoveFix)
		return false
	}
	return true
}

// validWaitingOn accepts "none" or "member:<user id of a workspace member>".
func (h *Handler) validWaitingOn(w http.ResponseWriter, r *http.Request, issue db.Issue, value string) bool {
	if value == "none" {
		return true
	}
	id, found := strings.CutPrefix(value, "member:")
	if found {
		memberID, err := util.ParseUUID(id)
		if err == nil {
			_, err = h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: memberID, WorkspaceID: issue.WorkspaceID})
		}
		if err == nil {
			return true
		}
	}
	writeError(w, http.StatusBadRequest, "waiting_on must be member:<id of a member of this workspace> or none")
	return false
}

// taskMadeTypedAsk reports whether the task already posted a decision request
// or intent, or a comment with a member mention, on this issue. Mentions are
// structural links; no comment text is judged. A failed read counts as no ask,
// so the rule fails toward telling the agent to declare.
func (h *Handler) taskMadeTypedAsk(r *http.Request, issue db.Issue, task db.AgentTaskQueue) bool {
	rows, err := h.DB.Query(r.Context(),
		`SELECT content, decision_request_id IS NOT NULL OR decision_intent_id IS NOT NULL FROM comment
		 WHERE source_task_id = $1 AND issue_id = $2 AND deleted_at IS NULL`, task.ID, issue.ID)
	if err != nil {
		slog.Warn("waiting on guard: read task comments failed", "error", err, "issue_id", uuidToString(issue.ID))
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		var decision bool
		if err := rows.Scan(&content, &decision); err != nil {
			return false
		}
		if decision || len(askguard.MentionedMembers(content)) > 0 {
			return true
		}
	}
	return false
}

// Typed asks (A4, CHE-7292): an agent comment with --ask <member> or a move with
// --waiting-on member:<id> names who acts next. The server stores one block
// event for it and the recovery pass sends the notice once the run ends, so the
// member hears of the ask even when the agent never mentions them. The event key
// is the comment id, or the issue, both statuses, the member and the issue
// revision of a move, so a repeat sends one notice. It follows the S4 and S5
// switches: off sends nothing. A chat-lane run or a call with no task sends
// nothing either, as there is no ended run to recover.
const (
	askNoticeKind  = "ask_notice"
	moveNoticeKind = "wait_notice"
)

func (h *Handler) typedNoticeOn(r *http.Request, workspaceID pgtype.UUID, task *db.AgentTaskQueue, modeOf func([]byte) (askguard.Mode, error)) bool {
	if task == nil || task.ChatSessionID.Valid {
		return false
	}
	workspace, err := h.Queries.GetWorkspace(r.Context(), workspaceID)
	if err != nil {
		slog.Warn("typed ask notice: read workspace failed", "error", err)
		return false
	}
	mode, err := modeOf(workspace.Settings)
	return err == nil && mode != askguard.ModeOff
}

// typedAskCommentMember returns the member a comment's --ask names when the
// comment is to be noticed, else an invalid UUID. S4 already validated ask.
func (h *Handler) typedAskCommentMember(r *http.Request, issue db.Issue, authorType string, task *db.AgentTaskQueue, ask string) pgtype.UUID {
	if authorType != "agent" || ask == "" || !h.typedNoticeOn(r, issue.WorkspaceID, task, askguard.WaitingAudienceModeFromSettings) {
		return pgtype.UUID{}
	}
	member, err := util.ParseUUID(ask)
	if err != nil {
		return pgtype.UUID{}
	}
	return member
}

// recordTypedAskComment stores the notice event of a committed --ask comment.
// Pass the transaction's Queries so the event commits with the comment.
func recordTypedAskComment(r *http.Request, queries *db.Queries, issue db.Issue, task *db.AgentTaskQueue, member pgtype.UUID, comment db.Comment) error {
	op := askguard.Op{IssueID: issue.ID, TaskID: task.ID, MemberID: member, Kind: askNoticeKind, Payload: []byte(uuidToString(comment.ID)), Source: comment.Content}
	return askguard.Guard{Queries: queries}.RecordNotice(r.Context(), issue.WorkspaceID, op)
}

// recordTypedWaitingMove stores the notice event of a move that named a member.
func (h *Handler) recordTypedWaitingMove(r *http.Request, prevStatus string, issue db.Issue, waitingOn *string) {
	if waitingOn == nil {
		return
	}
	id, found := strings.CutPrefix(*waitingOn, "member:")
	if !found {
		return
	}
	var task *db.AgentTaskQueue
	if t, ok := h.taskFromRequestHeader(r); ok {
		task = &t
	}
	member, err := util.ParseUUID(id)
	if err != nil || !h.typedNoticeOn(r, issue.WorkspaceID, task, askguard.WaitingOnModeFromSettings) {
		return
	}
	key := fmt.Sprintf("%s|%s|%s|%s|%d", uuidToString(issue.ID), prevStatus, issue.Status, id, issue.Revision)
	source := fmt.Sprintf("The run moved this issue from %s to %s and named you to act next.", prevStatus, issue.Status)
	op := askguard.Op{IssueID: issue.ID, TaskID: task.ID, MemberID: member, Kind: moveNoticeKind, Payload: []byte(key), Source: source}
	if err := (askguard.Guard{Queries: h.Queries}).RecordNotice(r.Context(), issue.WorkspaceID, op); err != nil {
		slog.Error("typed ask notice: record move failed; no notice will be sent", "error", err, "issue_id", uuidToString(issue.ID))
	}
}
