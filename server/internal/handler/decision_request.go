package handler

import (
	"context"
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
	Footer          string
	ParentID        pgtype.UUID
	EscalationClass string
}

type decisionRequestResult struct {
	Request         db.DecisionRequest
	Comment         db.Comment
	Message         db.ChatMessage
	ClearedComments []db.Comment
	CreatedSession  *db.ChatSession
	// PingItem is set only when the ping was recorded inside the request
	// transaction (single-call path); PingRecorded distinguishes "recorded,
	// no item for a departed recipient" from "not yet recorded".
	PingItem     *db.InboxItem
	PingRecorded bool
}

func (handler *Handler) createDecisionRequest(request *http.Request, issue db.Issue, input decisionRequestInput) (decisionRequestResult, error) {
	empty := decisionRequestResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request requires this issue's running task"}
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
	if input.NewDM && (input.SessionID.Valid || !input.HumanUserID.Valid) || strings.TrimSpace(input.Ask) == "" {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "a listed lane, a new DM or a named person, and a nonempty ask, are required"}
	}
	switch input.EscalationClass {
	case "reversible", "spend", "deploy", "publish", "irreversible", "product_direction":
	default:
		return empty, decisionIntentRefusal{http.StatusBadRequest, "invalid escalation class"}
	}
	intent, err := handler.Queries.GetDecisionIntentForTask(request.Context(), db.GetDecisionIntentForTaskParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: taskID, AgentID: agentID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	// A pending intent comment keeps the two-step flow; otherwise this call
	// opens, uses and seals its own window.
	if pending := err == nil && !intent.ConsumedAt.Valid && !intent.ClosedAt.Valid && !intent.SealedAt.Valid; pending {
		if input.SessionID.Valid == input.NewDM || input.ParentID.Valid {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "a listed lane or an explicit new DM, and no parent, are required with an open intent"}
		}
		return retryDecisionTx(request, func() (decisionRequestResult, error) {
			return handler.createDecisionRequestTx(request, issue, input, intent)
		})
	}
	if !input.HumanUserID.Valid {
		return empty, decisionIntentRefusal{http.StatusForbidden, "decision request requires a named person (for) or an open intent naming the lane's person"}
	}
	return retryDecisionTx(request, func() (decisionRequestResult, error) {
		return handler.createSingleCallDecisionTx(request, issue, input, agentID)
	})
}

func retryDecisionTx(request *http.Request, run func() (decisionRequestResult, error)) (decisionRequestResult, error) {
	deadlockRetried := false
	for attempt := 0; ; attempt++ {
		result, err := run()
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 2 {
			return result, err
		}
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "40P01" {
			if deadlockRetried {
				return decisionRequestResult{}, err
			}
			deadlockRetried = true
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-request.Context().Done():
			return decisionRequestResult{}, request.Context().Err()
		}
	}
}

// decisionAskContent is the ask comment body; the DM message carries the ask only.
func decisionAskContent(input decisionRequestInput) string {
	if strings.TrimSpace(input.Footer) == "" {
		return input.Ask
	}
	return input.Ask + "\n\n" + input.Footer
}

// lockDecisionLane locks an existing DM and proves it is one of the agent's
// listed decision lanes.
func lockDecisionLane(ctx context.Context, queries *db.Queries, workspaceID, sessionID, agentID pgtype.UUID) (db.ChatSession, error) {
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: workspaceID, ID: sessionID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ChatSession{}, deny
		}
		return db.ChatSession{}, err
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: sessionID, WorkspaceID: workspaceID})
	if err != nil {
		return db.ChatSession{}, err
	}
	if session.AgentID != agentID || session.Status != "active" || session.ChannelOriginAt.Valid || session.IsAgentIntro {
		return db.ChatSession{}, deny
	}
	lanes, err := queries.ListDecisionLanes(ctx, db.ListDecisionLanesParams{WorkspaceID: workspaceID, AgentID: agentID, CreatorID: session.CreatorID})
	if err != nil {
		return db.ChatSession{}, err
	}
	listed := false
	for _, lane := range lanes {
		if lane.ID == session.ID {
			listed = true
		}
	}
	if !listed {
		return db.ChatSession{}, deny
	}
	if _, err := queries.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); err == nil {
		return db.ChatSession{}, deny
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.ChatSession{}, err
	}
	return session, nil
}

// defaultDecisionLane picks, among the agent's listed lanes with the person,
// the lane of the latest request on this issue, else the newest lane that
// mentions this issue. The zero value means: open a new Decisions DM.
func defaultDecisionLane(ctx context.Context, queries *db.Queries, issue db.Issue, agentID, humanID pgtype.UUID) (pgtype.UUID, error) {
	lanes, err := queries.ListDecisionLanes(ctx, db.ListDecisionLanesParams{WorkspaceID: issue.WorkspaceID, AgentID: agentID, CreatorID: humanID})
	if err != nil || len(lanes) == 0 {
		return pgtype.UUID{}, err
	}
	latest, err := queries.LatestDecisionRequestLane(ctx, db.LatestDecisionRequestLaneParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: agentID, HumanUserID: humanID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, err
	}
	ids := make([]pgtype.UUID, 0, len(lanes))
	for _, lane := range lanes {
		if err == nil && lane.ID == latest {
			return lane.ID, nil
		}
		ids = append(ids, lane.ID)
	}
	mentions, err := queries.ListDecisionLaneMentions(ctx, db.ListDecisionLaneMentionsParams{WorkspaceID: issue.WorkspaceID, AgentID: agentID, CreatorID: humanID, SessionIds: ids})
	if err != nil {
		return pgtype.UUID{}, err
	}
	for _, lane := range lanes {
		for _, mention := range mentions {
			if mention.ChatSessionID == lane.ID && mention.IssueID == issue.ID {
				return lane.ID, nil
			}
		}
	}
	return pgtype.UUID{}, nil
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
		session, err = lockDecisionLane(ctx, queries, issue.WorkspaceID, input.SessionID, located.AgentID)
		if err != nil {
			return empty, err
		}
		if input.HumanUserID.Valid && session.CreatorID != input.HumanUserID {
			return empty, deny
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
		session, err = createDecisionDM(ctx, queries, issue, session)
		if err != nil {
			return empty, err
		}
		createdSession = &session
	}
	result, err := insertDecisionAsk(ctx, queries, issue, input, intent, session, createdSession != nil, root, intent.CommentID, dbid.NewV7(), false)
	if err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	result.CreatedSession = createdSession
	return result, nil
}

func createDecisionDM(ctx context.Context, queries *db.Queries, issue db.Issue, lane db.ChatSession) (db.ChatSession, error) {
	session, err := queries.CreateChatSession(ctx, db.CreateChatSessionParams{ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, AgentID: lane.AgentID, CreatorID: lane.CreatorID, Title: lane.Title})
	if err != nil {
		return session, err
	}
	_, err = queries.MarkChatSessionExplicitlyCreated(ctx, session.ID)
	return session, err
}

// insertDecisionAsk writes the request, ask comment, DM message and consumes
// the intent target; the caller commits. The ask comment is stamped with the
// intent only when it is the intent's own anchor (single-call path).
func insertDecisionAsk(ctx context.Context, queries *db.Queries, issue db.Issue, input decisionRequestInput, intent db.DecisionIntent, session db.ChatSession, newSession bool, root *db.Comment, parentID, commentID pgtype.UUID, anchorsIntent bool) (decisionRequestResult, error) {
	empty := decisionRequestResult{}
	messageID := dbid.NewV7()
	created, err := queries.CreateDecisionRequest(ctx, db.CreateDecisionRequestParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: intent.AgentID, HumanUserID: session.CreatorID, ChatSessionID: session.ID, PingMessageID: messageID, IssueCommentID: commentID, Ask: input.Ask, EscalationClass: input.EscalationClass, Origin: "asked", DecisionIntentID: intent.ID})
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
	commentParams := db.CreateCommentParams{ID: commentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "agent", AuthorID: intent.AgentID, SourceTaskID: intent.TaskID, ParentID: parentID, Content: decisionAskContent(input), Type: "comment", DecisionRequestID: created.ID, DecisionCommentKind: pgtype.Text{String: "ask", Valid: true}}
	if anchorsIntent {
		commentParams.DecisionIntentID = intent.ID
	}
	comment, err := queries.CreateComment(ctx, commentParams)
	if err != nil {
		return empty, err
	}
	message, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: messageID, ChatSessionID: session.ID, Role: "assistant", Content: input.Ask, MessageKind: pgtype.Text{String: "decision_request", Valid: true}, SentVia: pgtype.Text{String: "task_token", Valid: true}})
	if err != nil {
		return empty, err
	}
	if newSession {
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
		return empty, decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	}
	if err := queries.CompleteDecisionIntent(ctx, db.CompleteDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: intent.ID, RequestID: created.ID}); err != nil {
		return empty, err
	}
	return decisionRequestResult{Request: created, Comment: comment.Comment(), Message: message, ClearedComments: cleared}, nil
}

// createSingleCallDecisionTx is the request path without an intent comment: it
// runs the intent comment's checks inline against the named person, anchors a
// fresh intent on the ask comment itself, and consumes and seals it.
func (handler *Handler) createSingleCallDecisionTx(request *http.Request, issue db.Issue, input decisionRequestInput, agentID pgtype.UUID) (decisionRequestResult, error) {
	ctx := request.Context()
	empty := decisionRequestResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	sessionID := input.SessionID
	if !sessionID.Valid && !input.NewDM {
		if sessionID, err = defaultDecisionLane(ctx, queries, issue, agentID, input.HumanUserID); err != nil {
			return empty, err
		}
	}
	var session db.ChatSession
	if sessionID.Valid {
		if session, err = lockDecisionLane(ctx, queries, issue.WorkspaceID, sessionID, agentID); err != nil {
			return empty, err
		}
		if session.CreatorID != input.HumanUserID {
			return empty, deny
		}
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, issue.WorkspaceID, input.ParentID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && root != nil && root.IssueID != issue.ID {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "invalid parent comment"}
	}
	if err != nil {
		return empty, err
	}
	issue, agent, task, err := handler.authorizeDecisionTask(request, queries, issue, "agent", agentID)
	if err != nil {
		return empty, err
	}
	if agent.Kind == "system" {
		return empty, deny
	}
	if err := handler.checkDecisionTarget(request, queries, issue, agent, input.HumanUserID); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: issue.WorkspaceID, UserID: input.HumanUserID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	var createdSession *db.ChatSession
	if !sessionID.Valid {
		lane := db.ChatSession{AgentID: agent.ID, CreatorID: input.HumanUserID, Title: "Decisions: " + agent.Name}
		if session, err = createDecisionDM(ctx, queries, issue, lane); err != nil {
			return empty, err
		}
		createdSession = &session
	}
	if err := supersedeDecisionIntent(ctx, queries, issue, task.ID); err != nil {
		return empty, err
	}
	commentID := dbid.NewV7()
	intent, err := queries.CreateDecisionIntent(ctx, db.CreateDecisionIntentParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: task.ID, AgentID: agent.ID, CommentID: commentID})
	if err != nil {
		return empty, err
	}
	if _, err := queries.CreateDecisionIntentTarget(ctx, db.CreateDecisionIntentTargetParams{DecisionIntentID: intent.ID, HumanUserID: input.HumanUserID}); err != nil {
		return empty, err
	}
	result, err := insertDecisionAsk(ctx, queries, issue, input, intent, session, createdSession != nil, root, input.ParentID, commentID, true)
	if err != nil {
		return empty, err
	}
	// The ping commits with the request: a ping failure refuses the whole call.
	if result.PingItem, err = decisiontx.NotifyPingTx(ctx, queries, issue.WorkspaceID, result.Request.ID); err != nil {
		return empty, err
	}
	result.PingRecorded = true
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	result.CreatedSession = createdSession
	return result, nil
}
