package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type decisionRecordInput struct {
	ParentID   pgtype.UUID
	MessageIDs []pgtype.UUID
	// ContextMessageID is an earlier assistant message of the same DM, quoted
	// as the options the human's reply chose between.
	ContextMessageID pgtype.UUID
	// Excerpts are verbatim parts of the source message. When set, the record
	// quotes only them; the whole message stays behind a disclosure.
	Excerpts        []string
	Content         string
	EscalationClass string
	SourceCommentID pgtype.UUID
}

type decisionRecordResult struct {
	Request db.DecisionRequest
	Answer  db.DecisionAnswer
	Comment db.Comment
	Message db.ChatMessage
	// AnswerMessage is the task-less user row a card answer writes.
	AnswerMessage   db.ChatMessage
	ClearedComments []db.Comment
	Replayed        bool
}

func (handler *Handler) createAskedDecisionRecord(request *http.Request, issue db.Issue, input decisionRecordInput) (decisionRecordResult, error) {
	deadlockRetried := false
	for attempt := 0; ; attempt++ {
		result, err := handler.createAskedDecisionRecordTx(request, issue, input)
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 2 {
			return result, err
		}
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "40P01" {
			if deadlockRetried {
				return result, err
			}
			deadlockRetried = true
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-request.Context().Done():
			return decisionRecordResult{}, request.Context().Err()
		}
	}
}

func locateDecisionRecordContext(ctx context.Context, queries *db.Queries, issue db.Issue, parentID pgtype.UUID) (db.DecisionRequest, error) {
	seen := make(map[pgtype.UUID]bool)
	for parentID.Valid {
		if seen[parentID] {
			return db.DecisionRequest{}, decisionIntentRefusal{http.StatusBadRequest, "invalid decision parent chain"}
		}
		seen[parentID] = true
		parent, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: parentID, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (parent.IssueID != issue.ID || parent.DeletedAt.Valid) {
			return db.DecisionRequest{}, decisionIntentRefusal{http.StatusNotFound, "decision parent not found"}
		}
		if err != nil {
			return db.DecisionRequest{}, err
		}
		requestID := parent.DecisionRequestID
		if !requestID.Valid && parent.DecisionAnswerID.Valid {
			answer, err := queries.GetDecisionAnswer(ctx, db.GetDecisionAnswerParams{WorkspaceID: issue.WorkspaceID, ID: parent.DecisionAnswerID})
			if errors.Is(err, pgx.ErrNoRows) {
				return db.DecisionRequest{}, decisionIntentRefusal{http.StatusNotFound, "decision answer not found"}
			}
			if err != nil {
				return db.DecisionRequest{}, err
			}
			requestID = answer.DecisionRequestID
		}
		if requestID.Valid {
			decision, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: requestID})
			if errors.Is(err, pgx.ErrNoRows) || err == nil && decision.IssueID != issue.ID {
				return db.DecisionRequest{}, decisionIntentRefusal{http.StatusNotFound, "decision request not found"}
			}
			return decision, err
		}
		if parent.DecisionIntentID.Valid {
			return db.DecisionRequest{}, decisionIntentRefusal{http.StatusBadRequest, "reply to the person's decision ask, not the intent"}
		}
		parentID = parent.ParentID
	}
	return db.DecisionRequest{}, nil
}

func (handler *Handler) createAskedDecisionRecordTx(request *http.Request, issue db.Issue, input decisionRecordInput) (decisionRecordResult, error) {
	ctx := request.Context()
	empty := decisionRecordResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision record requires a running chat task or an assigned task on this issue"}
	if request.Header.Get("X-Actor-Source") != "task_token" {
		return empty, deny
	}
	taskID, err := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if err != nil {
		return empty, deny
	}
	agentID, err := util.ParseUUID(request.Header.Get("X-Agent-ID"))
	if err != nil {
		return empty, deny
	}
	task, err := handler.Queries.GetAgentTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (task.AgentID != agentID || task.Status != "running" || task.ChatSessionID.Valid == task.IssueID.Valid || task.IssueID.Valid && task.IssueID != issue.ID) {
		return empty, deny
	}
	if err != nil {
		return empty, err
	}
	located, err := locateDecisionRecordContext(ctx, handler.Queries, issue, input.ParentID)
	if err != nil {
		return empty, err
	}
	if (len(input.MessageIDs) > 0) == input.SourceCommentID.Valid || len(input.Content) > maxCommentContentBytes || input.SourceCommentID.Valid && input.ContextMessageID.Valid {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "a decision record requires source messages or one source comment, not both"}
	}
	if !located.ID.Valid && (task.IssueID.Valid || input.SourceCommentID.Valid) {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "an external answer requires --parent naming the ask"}
	}
	if !located.ID.Valid || input.EscalationClass != "" {
		switch input.EscalationClass {
		case "reversible", "spend", "deploy", "publish", "irreversible", "product_direction":
		default:
			return empty, decisionIntentRefusal{http.StatusBadRequest, "an unprompted record requires an explicit valid escalation class"}
		}
	}
	callingSessionID := task.ChatSessionID
	if !callingSessionID.Valid {
		callingSessionID = located.ChatSessionID
	}
	lockedSessionID := callingSessionID
	if len(input.MessageIDs) > 0 {
		first, err := handler.Queries.GetChatMessage(ctx, input.MessageIDs[0])
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source message is unavailable"}
		}
		if err != nil {
			return empty, err
		}
		lockedSessionID = first.ChatSessionID
	}
	// Runtime teardown unbinds user agents before runtime detachment; user DMs survive.
	// A comment merge can change trigger_comment_id, whose FK uses ON DELETE SET NULL.
	// The session-first fence and NOWAIT calling-task lock avoid both baseline suffixes.
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: issue.WorkspaceID, ID: lockedSessionID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, deny
	} else if err != nil {
		return empty, err
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: callingSessionID})
	if err != nil {
		return empty, err
	}
	if task.ChatSessionID.Valid && session.AgentID != agentID || session.Status != "active" || session.ChannelOriginAt.Valid || session.IsAgentIntro {
		return empty, deny
	}
	if _, err := queries.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); err == nil {
		return empty, deny
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: agentID})
	if err != nil {
		return empty, err
	}
	if agent.Kind == "system" {
		return empty, deny
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: issue.WorkspaceID, UserID: session.CreatorID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, deny
	} else if err != nil {
		return empty, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, issue.WorkspaceID, input.ParentID)
	if input.ParentID.Valid && (errors.Is(err, pgx.ErrNoRows) || err == nil && root == nil) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision parent not found"}
	}
	if err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID}); err != nil {
		return empty, err
	}
	issue, err = queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID})
	if err != nil {
		return empty, err
	}
	if !located.ID.Valid {
		category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
		if err != nil {
			return empty, err
		}
		if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
			return empty, decisionIntentRefusal{http.StatusForbidden, "decision record refuses a terminal issue"}
		}
	}
	if _, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: issue.WorkspaceID, ID: taskID}); err != nil {
		return empty, err
	}
	task, err = queries.GetAgentTask(ctx, taskID)
	if err != nil {
		return empty, err
	}
	if task.Status != "running" || task.AgentID != agentID || task.ChatSessionID.Valid && (task.ChatSessionID != callingSessionID || task.IssueID.Valid) || !task.ChatSessionID.Valid && task.IssueID != issue.ID {
		return empty, deny
	}
	if task.IssueID.Valid {
		if _, _, _, err := handler.authorizeDecisionTask(request, queries, issue, "agent", agentID); err != nil {
			return empty, err
		}
	}
	current, err := locateDecisionRecordContext(ctx, queries, issue, input.ParentID)
	if err != nil {
		return empty, err
	}
	if current.ID != located.ID {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision parent context changed; retry"}
	}
	commentID := dbid.NewV7()
	var decision db.DecisionRequest
	var pingAt pgtype.Timestamptz
	if current.ID.Valid {
		decision, err = queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: current.ID})
		if err != nil {
			return empty, err
		}
		if decision.HumanUserID != session.CreatorID || decision.IssueID != issue.ID {
			return empty, deny
		}
		lane, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: decision.ChatSessionID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (lane.CreatorID != decision.HumanUserID || lane.AgentID != decision.AskingAgentID || lane.Status != "active" || lane.ChannelOriginAt.Valid || lane.IsAgentIntro) {
			return empty, deny
		}
		if err != nil {
			return empty, err
		}
		if decision.Origin != "asked" || decision.Status != "open" {
			if decision.Origin == "asked" && decision.Status == "answered" {
				answer, err := queries.LockDecisionLiveAnswer(ctx, db.LockDecisionLiveAnswerParams{WorkspaceID: issue.WorkspaceID, ID: decision.ID})
				if err == nil && input.ParentID == decision.IssueCommentID && answer.AnsweredByTaskID == taskID && answer.SourceCommentID == input.SourceCommentID && slices.Equal(answer.ChatMessageIds, input.MessageIDs) && sameExcerpts(answer.Excerpted, answer.ReplyText.String, input.Excerpts) {
					comment, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: answer.IssueCommentID})
					if err != nil {
						return empty, err
					}
					message, err := queries.GetDecisionAnswerReference(ctx, db.GetDecisionAnswerReferenceParams{WorkspaceID: issue.WorkspaceID, AnswerID: answer.ID})
					if err != nil {
						return empty, err
					}
					return decisionRecordResult{Request: decision, Answer: answer, Comment: comment, Message: message, Replayed: true}, nil
				}
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return empty, err
				}
			}
			return empty, decisionIntentRefusal{http.StatusConflict, "this request cannot accept another answer"}
		}
		if input.EscalationClass != "" && input.EscalationClass != decision.EscalationClass {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "a record cannot change the request's class"}
		}
		ping, err := queries.GetChatMessage(ctx, decision.PingMessageID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && ping.ChatSessionID != decision.ChatSessionID {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "decision ping is unavailable"}
		}
		if err != nil {
			return empty, err
		}
		pingAt = ping.CreatedAt
	} else {
		if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: issue.WorkspaceID, UserID: session.CreatorID}); errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		} else if err != nil {
			return empty, err
		}
		decision, err = queries.CreateDecisionRequest(ctx, db.CreateDecisionRequestParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: agentID, HumanUserID: session.CreatorID, ChatSessionID: session.ID, IssueCommentID: commentID, EscalationClass: input.EscalationClass, Origin: "unprompted"})
		if err != nil {
			return empty, err
		}
	}
	if !task.StartedAt.Valid {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "decision source ordering is unavailable"}
	}
	seen := make(map[pgtype.UUID]bool)
	contents := make([]string, 0, len(input.MessageIDs))
	earliestSource := task.StartedAt.Time
	var sourceSessionID pgtype.UUID
	if input.SourceCommentID.Valid {
		source, err := queries.LockDecisionSourceComment(ctx, db.LockDecisionSourceCommentParams{WorkspaceID: issue.WorkspaceID, ID: input.SourceCommentID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (source.AuthorType != "member" || source.AuthorID != decision.HumanUserID || source.Type != "comment" || !source.CreatedAt.Time.After(pingAt.Time) || !source.CreatedAt.Time.Before(task.StartedAt.Time)) {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source comment is not an eligible answer by the request's human"}
		}
		if err != nil {
			return empty, err
		}
		if _, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: source.IssueID}); errors.Is(err, pgx.ErrNoRows) {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source issue is unavailable"}
		} else if err != nil {
			return empty, err
		}
		contents = append(contents, source.Content)
	}
	if len(input.MessageIDs) > 0 {
		sourceSession, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: lockedSessionID})
		if err != nil {
			return empty, err
		}
		if sourceSession.CreatorID != decision.HumanUserID || sourceSession.ChannelOriginAt.Valid || sourceSession.IsAgentIntro || !current.ID.Valid && sourceSession.ID != session.ID {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source DM is not eligible for this record"}
		}
		if _, err := queries.GetChannelChatSessionBindingBySessionAny(ctx, sourceSession.ID); err == nil {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "channel messages cannot supply decision answers"}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		sourceSessionID = sourceSession.ID
		locked, err := queries.LockDecisionSourceMessages(ctx, db.LockDecisionSourceMessagesParams{WorkspaceID: issue.WorkspaceID, MessageIds: input.MessageIDs})
		if err != nil {
			return empty, err
		}
		if len(locked) != len(input.MessageIDs) {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source messages must exist and be distinct"}
		}
	}
	var boundMessages []db.ChatMessage
	for _, messageID := range input.MessageIDs {
		if !messageID.Valid || seen[messageID] {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source messages must be distinct"}
		}
		seen[messageID] = true
		message, err := queries.GetChatMessage(ctx, messageID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		if err != nil || message.ChatSessionID != sourceSessionID || message.Role != "user" || message.MessageKind != "message" || message.SentVia.String != "interactive_jwt" || message.ChannelIngested || pingAt.Valid && !message.CreatedAt.Time.After(pingAt.Time) || !message.CreatedAt.Time.Before(task.StartedAt.Time) {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source messages are not eligible for this answering turn"}
		}
		bound, err := loadDecisionAnswerBinding(ctx, queries, issue.WorkspaceID, message.DecisionAnswerID)
		if err != nil {
			return empty, err
		}
		if bound.ID.Valid {
			if bound.DecisionRequestID != decision.ID {
				return empty, decisionIntentRefusal{http.StatusBadRequest, "source message explicitly selects another decision request"}
			}
			if !bound.DisclaimedAt.Valid {
				return empty, decisionIntentRefusal{http.StatusBadRequest, "source message already answers its explicitly selected request"}
			}
			boundMessages = append(boundMessages, message)
		}
		contents = append(contents, message.Content)
		if message.CreatedAt.Time.Before(earliestSource) {
			earliestSource = message.CreatedAt.Time
		}
	}
	snapshot := strings.Join(contents, "\n\n")
	quote, replyText := blockQuote(snapshot), snapshot
	var spans []string
	var first excerptMatch
	if len(input.Excerpts) > 0 {
		spans, first, err = verbatimExcerpts(contents, input.Excerpts)
		if err != nil {
			return empty, err
		}
		quote, replyText = quoteExcerpts(spans), excerptReplyText(spans)
	}
	answeredLabel := ""
	if input.ContextMessageID.Valid {
		offered, err := queries.GetChatMessage(ctx, input.ContextMessageID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		if err != nil || offered.ChatSessionID != sourceSessionID || offered.Role != "assistant" || offered.MessageKind != "message" || !offered.CreatedAt.Time.Before(earliestSource) {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "context message is not an earlier assistant message of this DM"}
		}
		if first.unique {
			answeredLabel = offeredLabel(offered.Content, first.source, first.start)
		}
	}
	if input.Content != "" {
		quote = input.Content + "\n\n" + quote
	}
	if len(quote) > maxCommentContentBytes {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "decision quote is too long"}
	}
	result, err := handler.persistDecisionRecord(ctx, queries, issue, decision, input, decisionRecordSource{
		TaskID: taskID, AgentID: agentID, SessionID: session.ID, SourceSessionID: sourceSessionID,
		CommentID: commentID, Snapshot: snapshot, Quote: quote, ReplyText: replyText, Excerpted: len(spans) > 0, AnsweredLabel: answeredLabel,
	}, root)
	if err != nil {
		return empty, err
	}
	for _, message := range boundMessages {
		if _, err := queries.BindChatMessageDecisionAnswer(ctx, db.BindChatMessageDecisionAnswerParams{
			WorkspaceID: issue.WorkspaceID, SessionID: sourceSessionID, MessageID: message.ID, AnswerID: result.Answer.ID, PreviousAnswerID: message.DecisionAnswerID,
		}); err != nil {
			return empty, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return result, nil
}

type decisionRecordSource struct {
	TaskID, AgentID, SessionID, SourceSessionID, CommentID pgtype.UUID
	Snapshot, Quote, ReplyText                             string
	Excerpted                                              bool
	AnsweredLabel                                          string
}

func (handler *Handler) persistDecisionRecord(ctx context.Context, queries *db.Queries, issue db.Issue, decision db.DecisionRequest, input decisionRecordInput, source decisionRecordSource, root *db.Comment) (decisionRecordResult, error) {
	empty := decisionRecordResult{}
	effectiveAt, err := queries.DecisionRecordEffectiveAt(ctx, db.DecisionRecordEffectiveAtParams{WorkspaceID: issue.WorkspaceID, ID: decision.ID})
	if err != nil {
		return empty, err
	}
	messageIDs := input.MessageIDs
	if messageIDs == nil {
		messageIDs = []pgtype.UUID{}
	}
	answer, err := queries.CreateDecisionAnswer(ctx, db.CreateDecisionAnswerParams{DecisionRequestID: decision.ID, AnsweredByTaskID: source.TaskID, SourceChatSessionID: source.SourceSessionID, ChatMessageIds: messageIDs, SourceCommentID: input.SourceCommentID, ContentSnapshot: source.Snapshot, EffectiveAt: effectiveAt, IssueCommentID: source.CommentID, ReplyText: pgtype.Text{String: source.ReplyText, Valid: true}, ContextMessageID: input.ContextMessageID, Excerpted: source.Excerpted, AnsweredLabel: pgtype.Text{String: source.AnsweredLabel, Valid: source.AnsweredLabel != ""}})
	if err != nil {
		return empty, err
	}
	var cleared []db.Comment
	if root != nil {
		cleared, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return empty, err
		}
	}
	comment, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: source.CommentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "agent", AuthorID: source.AgentID, SourceTaskID: source.TaskID, ParentID: input.ParentID, Content: source.Quote, Type: "comment", DecisionAnswerID: answer.ID, DecisionCommentKind: pgtype.Text{String: "quote", Valid: true}})
	if err != nil {
		return empty, err
	}
	workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return empty, err
	}
	note := fmt.Sprintf("Decision recorded for [%s-%d](mention://issue/%s).\nClass: %s.\nPending until this answering turn finishes.\nOpen issue: %s", workspace.IssuePrefix, issue.Number, uuidToString(issue.ID), decision.EscalationClass, util.WorkspaceIssuePath(workspace.Slug, uuidToString(issue.ID)))
	message, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: source.SessionID, Role: "assistant", Content: note, MessageKind: pgtype.Text{String: "decision_reference", Valid: true}, SentVia: pgtype.Text{String: "task_token", Valid: true}, DecisionAnswerID: answer.ID})
	if err != nil {
		return empty, err
	}
	if err := queries.TouchChatSession(ctx, source.SessionID); err != nil {
		return empty, err
	}
	decision, err = queries.MarkDecisionRequestAnswered(ctx, db.MarkDecisionRequestAnsweredParams{WorkspaceID: issue.WorkspaceID, ID: decision.ID})
	if err != nil {
		return empty, err
	}
	// The ping that announced this request has done its job.
	if _, err := queries.MarkInboxReadByDecisionRequest(ctx, db.MarkInboxReadByDecisionRequestParams{WorkspaceID: issue.WorkspaceID, DecisionRequestID: uuidToString(decision.ID)}); err != nil {
		return empty, err
	}
	return decisionRecordResult{Request: decision, Answer: answer, Comment: comment.Comment(), Message: message, ClearedComments: cleared}, nil
}

func blockQuote(text string) string {
	return "> " + strings.ReplaceAll(text, "\n", "\n> ")
}
