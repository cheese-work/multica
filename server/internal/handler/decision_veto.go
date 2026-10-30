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
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) createDecisionVetoComment(writer http.ResponseWriter, request *http.Request, issue db.Issue, input CreateCommentRequest) {
	if input.ParentID == nil || strings.TrimSpace(input.Content) == "" || len(input.Content) > maxCommentContentBytes || input.DecisionIntent || input.DecisionRecord || input.Type != "comment" || len(input.MessageIDs) > 0 || input.ContextMessageID != "" || len(input.Excerpts) > 0 || input.EscalationClass != "" || len(input.AttachmentIDs) > 0 || len(input.SuppressAgentIDs) > 0 || len(input.SteerTaskIDs) > 0 {
		writeError(writer, http.StatusBadRequest, "decision veto requires a stamped parent and a reason, without other decision, attachment, steering or suppression inputs")
		return
	}
	parentID, ok := parseUUIDOrBadRequest(writer, *input.ParentID, "parent_id")
	if !ok {
		return
	}
	if handler.rejectMissingRequiredSourceLink(writer, request, "agent", true, issue, input.Content, "content") {
		return
	}
	var result decisiontx.Withdrawal
	var err error
	deadlockRetried := false
	for attempt := 0; ; attempt++ {
		result, err = handler.vetoDecisionAnswerTx(request, issue, parentID, input.Content)
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 2 {
			break
		}
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "40P01" {
			if deadlockRetried {
				break
			}
			deadlockRetried = true
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-request.Context().Done():
			writeDecisionError(writer, request.Context().Err(), "decision veto")
			return
		}
	}
	if err != nil {
		writeDecisionError(writer, err, "decision veto")
		return
	}
	workspaceID, agentID := uuidToString(issue.WorkspaceID), request.Header.Get("X-Agent-ID")
	for _, comment := range result.Comments {
		handler.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{"comment": commentToResponse(comment, nil, nil), "issue_title": issue.Title, "issue_status": issue.Status})
	}
	handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "agent", agentID)
	handler.publishDecisionAnswerUpdated(workspaceID, "agent", agentID, result.Answer.ID, result.Request.IssueID, decisionStateDisclaimed)
	writeJSON(writer, http.StatusCreated, commentToResponse(result.Comments[0], nil, nil))
}

func (handler *Handler) vetoDecisionAnswerTx(request *http.Request, issue db.Issue, parentID pgtype.UUID, content string) (decisiontx.Withdrawal, error) {
	ctx := request.Context()
	empty := decisiontx.Withdrawal{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision veto requires a running assigned issue task or a running task of the answer's source DM"}
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
	parent, err := handler.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: parentID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (parent.DeletedAt.Valid || parent.IssueID != issue.ID) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision quote not found"}
	}
	if err != nil {
		return empty, err
	}
	if !parent.DecisionAnswerID.Valid {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "decision veto parent must carry an answer stamp"}
	}
	answer, err := handler.Queries.GetDecisionAnswer(ctx, db.GetDecisionAnswerParams{WorkspaceID: issue.WorkspaceID, ID: parent.DecisionAnswerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision answer not found"}
	}
	if err != nil {
		return empty, err
	}
	located, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: answer.DecisionRequestID})
	if err != nil {
		return empty, err
	}
	if located.IssueID != issue.ID {
		return empty, deny
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
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: issue.WorkspaceID, ID: located.ChatSessionID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision lane not found"}
	} else if err != nil {
		return empty, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, issue.WorkspaceID, parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision quote not found"}
	}
	if err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID}); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: issue.WorkspaceID, ID: taskID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, deny
	} else if err != nil {
		return empty, err
	}
	task, err := queries.GetAgentTask(ctx, taskID)
	if err != nil {
		return empty, err
	}
	if task.AgentID != agentID || task.Status != "running" {
		return empty, deny
	}
	issue, err = queries.GetIssue(ctx, issue.ID)
	if err != nil {
		return empty, err
	}
	reason, suppress := "record_retracted", false
	if task.IssueID == issue.ID && !task.ChatSessionID.Valid {
		assigned := issue.AssigneeType.String == "agent" && issue.AssigneeID == agentID
		suppress = assigned
		if issue.AssigneeType.String == "squad" && issue.AssigneeID.Valid {
			squad, err := queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
			if errors.Is(err, pgx.ErrNoRows) || err == nil && squad.ArchivedAt.Valid {
				return empty, deny
			}
			if err != nil {
				return empty, err
			}
			assigned, err = queries.IsSquadMember(ctx, db.IsSquadMemberParams{SquadID: squad.ID, MemberType: "agent", MemberID: agentID})
			if err != nil {
				return empty, err
			}
			suppress = squad.LeaderID == agentID
		}
		if !assigned {
			return empty, deny
		}
		reason = "agent_veto"
	} else if task.IssueID.Valid || task.ChatSessionID != answer.SourceChatSessionID {
		return empty, deny
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: located.ID})
	if err != nil {
		return empty, err
	}
	if current.ChatSessionID != located.ChatSessionID || current.IssueID != issue.ID || current.IssueCommentID != located.IssueCommentID {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision context changed; retry"}
	}
	answer, err = queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: issue.WorkspaceID, ID: answer.ID})
	if err != nil {
		return empty, err
	}
	if answer.DisclaimedAt.Valid {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision answer is already withdrawn"}
	}
	parent, err = queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: parentID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (parent.DeletedAt.Valid || parent.IssueID != issue.ID) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision quote not found"}
	}
	if err != nil {
		return empty, err
	}
	if parent.DecisionAnswerID != answer.ID {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision quote changed; retry"}
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: current.ChatSessionID})
	if err != nil {
		return empty, err
	}
	comment, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ParentID: parentID, AuthorType: "agent", AuthorID: agentID, SourceTaskID: taskID, Type: "comment", Content: content, DecisionAnswerID: answer.ID, DecisionCommentKind: pgtype.Text{String: "veto", Valid: true}})
	if err != nil {
		return empty, err
	}
	result, err := decisiontx.WithdrawByAgent(ctx, queries, current, answer, session.Status, reason, comment.Comment(), suppress)
	if err != nil {
		return empty, err
	}
	if root != nil {
		result.ClearedComments, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return empty, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return result, nil
}
