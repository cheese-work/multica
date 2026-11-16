package handler

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The named human's reply directly under the ask comment of an open kind=action
// request answers it (CHE-2394 P5). A reply anywhere else, or by anyone else,
// is a plain comment; the issue-wide human-reply wakeup still covers it.

// actionReplyCandidate is the open kind=action request that a member's reply
// under parent may answer, or the zero value when none applies. It is a cheap
// read; the locked recheck is lockActionReplyRequest.
func (handler *Handler) actionReplyCandidate(ctx context.Context, issue db.Issue, parent *db.Comment, authorType string, userID pgtype.UUID) (db.DecisionRequest, error) {
	if authorType != "member" || parent == nil {
		return db.DecisionRequest{}, nil
	}
	ask, err := handler.Queries.FindOpenActionRequestByAsk(ctx, db.FindOpenActionRequestByAskParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, IssueCommentID: parent.ID, HumanUserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.DecisionRequest{}, nil
	}
	return ask, err
}

// lockActionReplySession takes the locks that precede the thread lock in the
// card answer's order: the workspace chat-create lock, then the request's DM lane.
func lockActionReplySession(ctx context.Context, queries *db.Queries, ask db.DecisionRequest) error {
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, ask.WorkspaceID); err != nil {
		return err
	}
	_, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: ask.WorkspaceID, ID: ask.ChatSessionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// lockActionReplyRequest locks the issue and then the request, after the thread
// lock, and reports whether the reply still binds. A request that was answered,
// cancelled or moved since the read, or whose issue is closed, leaves the reply
// a plain comment.
func lockActionReplyRequest(ctx context.Context, queries *db.Queries, ask db.DecisionRequest) (db.DecisionRequest, bool, error) {
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: ask.WorkspaceID, ID: ask.IssueID}); err != nil {
		return ask, false, err
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: ask.WorkspaceID, ID: ask.ID})
	if err != nil {
		return ask, false, err
	}
	if current.Kind != "action" || current.Status != "open" || current.Origin != "asked" || current.IssueCommentID != ask.IssueCommentID || current.HumanUserID != ask.HumanUserID {
		return current, false, nil
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: ask.WorkspaceID, ID: ask.IssueID})
	if err != nil {
		return current, false, err
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil {
		return current, false, err
	}
	return current, category != issuestatus.CategoryDone && category != issuestatus.CategoryClosed, nil
}

// bindActionReply records the reply as the request's answer. The answer is a
// task-less record sourced from the reply comment; it takes effect at once and
// is guarded by status and expires_at in one update.
func (handler *Handler) bindActionReply(ctx context.Context, queries *db.Queries, issue db.Issue, ask db.DecisionRequest, reply db.Comment) (decisionRecordResult, error) {
	return handler.persistDecisionRecord(ctx, queries, issue, ask, decisionRecordInput{
		ParentID: ask.IssueCommentID, SourceCommentID: reply.ID,
	}, decisionRecordSource{
		AgentID: ask.AskingAgentID, SessionID: ask.ChatSessionID,
		CommentID: dbid.NewV7(), Snapshot: reply.Content, Quote: blockQuote(reply.Content), ReplyText: reply.Content,
		AnsweredLabel: actionAnswerReply,
	}, nil)
}

// publishActionReplyAnswer sends the events a bound reply adds after commit.
func (handler *Handler) publishActionReplyAnswer(ctx context.Context, issue db.Issue, ask db.DecisionRequest, result decisionRecordResult) {
	workspaceID, agentID := uuidToString(issue.WorkspaceID), uuidToString(ask.AskingAgentID)
	response := commentToResponse(result.Comment, nil, nil)
	response.DecisionCommentFields = handler.decisionCommentFields(ctx, issue.WorkspaceID, []db.Comment{result.Comment}, pgtype.UUID{})[response.ID]
	handler.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{"comment": response, "issue_title": issue.Title, "issue_status": issue.Status})
	handler.publishDecisionAnswerUpdated(workspaceID, "agent", agentID, result.Answer.ID, issue.ID, decisionAnswerState(result.Answer.WokenAt, result.Answer.DisclaimedAt))
	handler.publishChatFor(protocol.EventChatMessage, workspaceID, "agent", agentID, uuidToString(ask.HumanUserID), uuidToString(ask.ChatSessionID), protocol.ChatMessagePayload{
		MessageKind:   result.Message.MessageKind,
		ChatSessionID: uuidToString(ask.ChatSessionID), MessageID: uuidToString(result.Message.ID), Role: result.Message.Role, Content: result.Message.Content, CreatedAt: timestampToString(result.Message.CreatedAt),
	})
}
