package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	ParentID        pgtype.UUID
	MessageIDs      []pgtype.UUID
	Content         string
	EscalationClass string
}

type decisionRecordResult struct {
	Request         db.DecisionRequest
	Answer          db.DecisionAnswer
	Comment         db.Comment
	Message         db.ChatMessage
	ClearedComments []db.Comment
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
	deny := decisionIntentRefusal{http.StatusForbidden, "decision record requires this DM's running chat task"}
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
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (task.AgentID != agentID || !task.ChatSessionID.Valid || task.IssueID.Valid || task.Status != "running") {
		return empty, deny
	}
	if err != nil {
		return empty, err
	}
	located, err := locateDecisionRecordContext(ctx, handler.Queries, issue, input.ParentID)
	if err != nil {
		return empty, err
	}
	if len(input.MessageIDs) == 0 || len(input.Content) > maxCommentContentBytes {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "a decision record requires source messages"}
	}
	if !located.ID.Valid || input.EscalationClass != "" {
		switch input.EscalationClass {
		case "reversible", "spend", "deploy", "publish", "irreversible", "product_direction":
		default:
			return empty, decisionIntentRefusal{http.StatusBadRequest, "an unprompted record requires an explicit valid escalation class"}
		}
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
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: issue.WorkspaceID, ID: task.ChatSessionID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, deny
	} else if err != nil {
		return empty, err
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: task.ChatSessionID})
	if err != nil {
		return empty, err
	}
	if session.AgentID != agentID || session.Status != "active" || session.ChannelOriginAt.Valid || session.IsAgentIntro {
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
	if task.Status != "running" || task.AgentID != agentID || task.ChatSessionID != session.ID || task.IssueID.Valid {
		return empty, deny
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
		if decision.Origin != "asked" || decision.Status != "open" {
			return empty, decisionIntentRefusal{http.StatusConflict, "this request cannot accept another answer"}
		}
		if decision.ChatSessionID != session.ID || decision.HumanUserID != session.CreatorID || decision.IssueID != issue.ID {
			return empty, deny
		}
		if input.EscalationClass != "" && input.EscalationClass != decision.EscalationClass {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "a record cannot change the request's class"}
		}
		ping, err := queries.GetChatMessage(ctx, decision.PingMessageID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && ping.ChatSessionID != session.ID {
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
	for _, messageID := range input.MessageIDs {
		if !messageID.Valid || seen[messageID] {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source messages must be distinct"}
		}
		seen[messageID] = true
		message, err := queries.GetChatMessage(ctx, messageID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		if err != nil || message.ChatSessionID != session.ID || message.Role != "user" || message.MessageKind != "message" || message.SentVia.String != "interactive_jwt" || message.ChannelIngested || pingAt.Valid && !message.CreatedAt.Time.After(pingAt.Time) || !message.CreatedAt.Time.Before(task.StartedAt.Time) {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "source messages are not eligible for this answering turn"}
		}
		contents = append(contents, message.Content)
	}
	snapshot := strings.Join(contents, "\n\n")
	quote := "> " + strings.ReplaceAll(snapshot, "\n", "\n> ")
	if input.Content != "" {
		quote = input.Content + "\n\n" + quote
	}
	if len(quote) > maxCommentContentBytes {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "decision quote is too long"}
	}
	effectiveAt, err := queries.DecisionRecordEffectiveAt(ctx, db.DecisionRecordEffectiveAtParams{WorkspaceID: issue.WorkspaceID, ID: decision.ID})
	if err != nil {
		return empty, err
	}
	answer, err := queries.CreateDecisionAnswer(ctx, db.CreateDecisionAnswerParams{DecisionRequestID: decision.ID, AnsweredByTaskID: taskID, SourceChatSessionID: session.ID, ChatMessageIds: input.MessageIDs, ContentSnapshot: snapshot, EffectiveAt: effectiveAt, IssueCommentID: commentID})
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
	comment, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: commentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "agent", AuthorID: agentID, SourceTaskID: taskID, ParentID: input.ParentID, Content: quote, Type: "comment", DecisionAnswerID: answer.ID, DecisionCommentKind: pgtype.Text{String: "quote", Valid: true}})
	if err != nil {
		return empty, err
	}
	workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return empty, err
	}
	note := fmt.Sprintf("Decision recorded for [%s-%d](mention://issue/%s).\nClass: %s.\nPending until this DM turn finishes.\nOpen issue: /issue/%s\nDisclaim: /api/decision-answers/%s/disclaim", workspace.IssuePrefix, issue.Number, uuidToString(issue.ID), decision.EscalationClass, uuidToString(issue.ID), uuidToString(answer.ID))
	message, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: note, MessageKind: pgtype.Text{String: "decision_reference", Valid: true}, SentVia: pgtype.Text{String: "task_token", Valid: true}})
	if err != nil {
		return empty, err
	}
	if err := queries.TouchChatSession(ctx, session.ID); err != nil {
		return empty, err
	}
	decision, err = queries.MarkDecisionRequestAnswered(ctx, db.MarkDecisionRequestAnsweredParams{WorkspaceID: issue.WorkspaceID, ID: decision.ID})
	if err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return decisionRecordResult{Request: decision, Answer: answer, Comment: comment.Comment(), Message: message, ClearedComments: cleared}, nil
}
