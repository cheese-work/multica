package handler

import (
	"errors"
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

type decisionRequestInput struct {
	SessionID       pgtype.UUID
	NewDM           bool
	HumanUserID     pgtype.UUID
	Ask             string
	EscalationClass string
}

type decisionRequestResult struct {
	Request          db.DecisionRequest
	Comment          db.Comment
	AttachedToIntent bool
	Message          db.ChatMessage
	ClearedComments  []db.Comment
	CreatedSession   *db.ChatSession
}

func (handler *Handler) createDecisionRequest(request *http.Request, issue db.Issue, input decisionRequestInput) (decisionRequestResult, error) {
	empty := decisionRequestResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request requires this issue's running task and an open intent naming the lane's person"}
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
	if input.SessionID.Valid == input.NewDM || input.NewDM && !input.HumanUserID.Valid || strings.TrimSpace(input.Ask) == "" {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "a listed lane or an explicit new DM for a person, and a nonempty ask, are required"}
	}
	switch input.EscalationClass {
	case "reversible", "spend", "deploy", "publish", "irreversible", "product_direction":
	default:
		return empty, decisionIntentRefusal{http.StatusBadRequest, "invalid escalation class"}
	}
	intent, err := handler.Queries.GetDecisionIntentForTask(request.Context(), db.GetDecisionIntentForTaskParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: taskID, AgentID: agentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, deny
	}
	if err != nil {
		return empty, err
	}
	deadlockRetried := false
	for attempt := 0; ; attempt++ {
		result, err := handler.createDecisionRequestTx(request, issue, input, intent)
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 2 {
			return result, err
		}
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "40P01" {
			if deadlockRetried {
				return empty, err
			}
			deadlockRetried = true
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-request.Context().Done():
			return empty, request.Context().Err()
		}
	}
}

func (handler *Handler) createDecisionRequestTx(request *http.Request, issue db.Issue, input decisionRequestInput, located db.DecisionIntent) (decisionRequestResult, error) {
	ctx := request.Context()
	empty := decisionRequestResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	var createdSession *db.ChatSession
	var session db.ChatSession
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	if input.NewDM {
		agent, targetID, err := handler.decisionLaneWindow(request, queries, issue, input.HumanUserID)
		if err != nil {
			return empty, err
		}
		session = db.ChatSession{AgentID: agent.ID, CreatorID: targetID, Title: "Decisions: " + agent.Name}
	} else {
		if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: issue.WorkspaceID, ID: input.SessionID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return empty, deny
			}
			return empty, err
		}
		session, err = queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: input.SessionID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return empty, err
		}
		if session.AgentID != located.AgentID || session.Status != "active" || session.ChannelOriginAt.Valid || session.IsAgentIntro {
			return empty, deny
		}
		lanes, err := queries.ListDecisionLanes(ctx, db.ListDecisionLanesParams{WorkspaceID: issue.WorkspaceID, AgentID: located.AgentID, CreatorID: session.CreatorID})
		if err != nil {
			return empty, err
		}
		listed := false
		for _, lane := range lanes {
			if lane.ID == session.ID {
				listed = true
			}
		}
		if !listed {
			return empty, deny
		}
		if _, err := queries.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); err == nil {
			return empty, deny
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, issue.WorkspaceID, located.CommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	issue, err = queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID})
	if err != nil {
		return empty, err
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil {
		return empty, err
	}
	if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return empty, decisionIntentRefusal{http.StatusForbidden, "decision request refuses a terminal issue"}
	}
	if _, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: issue.WorkspaceID, ID: located.TaskID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: issue.WorkspaceID, UserID: session.CreatorID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	intent, err := queries.LockDecisionIntent(ctx, db.LockDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: located.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, deny
	}
	if err != nil {
		return empty, err
	}
	if intent.IssueID != issue.ID || intent.TaskID != located.TaskID || intent.AgentID != located.AgentID || intent.CommentID != located.CommentID {
		return empty, deny
	}
	agent, _, windowError := handler.decisionLaneWindow(request, queries, issue, session.CreatorID)
	if err := decisiontx.ValidateIntent(ctx, transaction, intent); err != nil {
		if errors.Is(err, decisiontx.ErrIntentClosed) {
			handler.DecisionWorker.Notify()
			return empty, decisionIntentRefusal{http.StatusForbidden, err.Error()}
		}
		return empty, err
	}
	if windowError != nil {
		return empty, windowError
	}
	if root == nil || agent.Kind == "system" {
		return empty, deny
	}
	tag, err := queries.GetComment(ctx, intent.CommentID)
	if err != nil {
		return empty, err
	}
	if tag.WorkspaceID != issue.WorkspaceID || tag.IssueID != issue.ID || tag.AuthorType != "agent" || tag.AuthorID != intent.AgentID || tag.SourceTaskID != intent.TaskID || tag.DecisionIntentID != intent.ID {
		return empty, deny
	}
	target, err := queries.LockDecisionIntentTarget(ctx, db.LockDecisionIntentTargetParams{WorkspaceID: issue.WorkspaceID, DecisionIntentID: intent.ID, HumanUserID: session.CreatorID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && target.ConsumedAt.Valid) {
		return empty, deny
	}
	if err != nil {
		return empty, err
	}
	if input.NewDM {
		session, err = queries.CreateChatSession(ctx, db.CreateChatSessionParams{ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, AgentID: session.AgentID, CreatorID: session.CreatorID, Title: session.Title})
		if err != nil {
			return empty, err
		}
		if _, err := queries.MarkChatSessionExplicitlyCreated(ctx, session.ID); err != nil {
			return empty, err
		}
		createdSession = &session
	}
	// An ask identical to the intent comment is that comment: reuse it instead of posting a second copy.
	// A comment carries one request, so later targets of a group intent get their own ask comment.
	attach := !tag.DecisionRequestID.Valid && strings.TrimSpace(tag.Content) == strings.TrimSpace(input.Ask)
	commentID, messageID := dbid.NewV7(), dbid.NewV7()
	if attach {
		commentID = tag.ID
	}
	created, err := queries.CreateDecisionRequest(ctx, db.CreateDecisionRequestParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: intent.AgentID, HumanUserID: session.CreatorID, ChatSessionID: session.ID, PingMessageID: messageID, IssueCommentID: commentID, Ask: input.Ask, EscalationClass: input.EscalationClass, Origin: "asked", DecisionIntentID: intent.ID})
	if err != nil {
		return empty, err
	}
	cleared, err := queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return empty, err
	}
	var comment db.Comment
	if attach {
		comment, err = queries.AttachDecisionAskToComment(ctx, db.AttachDecisionAskToCommentParams{WorkspaceID: issue.WorkspaceID, CommentID: tag.ID, DecisionRequestID: created.ID})
	} else {
		var row db.CreateCommentRow
		row, err = queries.CreateComment(ctx, db.CreateCommentParams{ID: commentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "agent", AuthorID: intent.AgentID, SourceTaskID: intent.TaskID, ParentID: intent.CommentID, Content: input.Ask, Type: "comment", DecisionRequestID: created.ID, DecisionCommentKind: pgtype.Text{String: "ask", Valid: true}})
		comment = row.Comment()
	}
	if err != nil {
		return empty, err
	}
	message, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: messageID, ChatSessionID: session.ID, Role: "assistant", Content: input.Ask, MessageKind: pgtype.Text{String: "decision_request", Valid: true}, SentVia: pgtype.Text{String: "task_token", Valid: true}})
	if err != nil {
		return empty, err
	}
	if createdSession != nil {
		if err := queries.MarkChatSessionReadBefore(ctx, db.MarkChatSessionReadBeforeParams{ID: session.ID, MessageCreatedAt: message.CreatedAt}); err != nil {
			return empty, err
		}
	}
	if err := queries.TouchChatSession(ctx, session.ID); err != nil {
		return empty, err
	}
	consumed, err := queries.ConsumeDecisionIntentTarget(ctx, db.ConsumeDecisionIntentTargetParams{WorkspaceID: issue.WorkspaceID, DecisionIntentID: intent.ID, HumanUserID: session.CreatorID, RequestID: created.ID})
	if err != nil {
		return empty, err
	}
	if consumed != 1 {
		return empty, deny
	}
	if err := queries.CompleteDecisionIntent(ctx, db.CompleteDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: intent.ID, RequestID: created.ID}); err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return decisionRequestResult{Request: created, Comment: comment, AttachedToIntent: attach, Message: message, ClearedComments: cleared, CreatedSession: createdSession}, nil
}
