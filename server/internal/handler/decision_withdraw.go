package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) WithdrawDecision(writer http.ResponseWriter, request *http.Request) {
	workspaceID := handler.resolveWorkspaceID(request)
	if _, ok := handler.requireWorkspaceMember(writer, request, workspaceID, "workspace not found"); !ok {
		return
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(writer, workspaceID, "workspace_id")
	if !ok {
		return
	}
	requestID, ok := parseUUIDOrBadRequest(writer, chi.URLParam(request, "id"), "id")
	if !ok {
		return
	}
	stored, err := handler.Queries.GetDecisionRequest(request.Context(), db.GetDecisionRequestParams{WorkspaceID: workspaceUUID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(writer, http.StatusNotFound, "decision request not found")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to read decision request")
		return
	}
	if _, ok := handler.loadIssueForUser(writer, request, uuidToString(stored.IssueID)); !ok {
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxCommentContentBytes)).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid withdrawal")
		return
	}
	if strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 2000 {
		writeError(writer, http.StatusBadRequest, "reason must contain 1 to 2000 bytes of nonblank text")
		return
	}
	result, err := retryDecisionTx(request, func() (decisionRequestResult, error) {
		return handler.withdrawDecisionRequestTx(request, stored, input.Reason)
	})
	if err != nil {
		writeDecisionError(writer, err, "decision withdrawal")
		return
	}
	if !result.Replayed {
		actorType, actorID := result.Comment.AuthorType, uuidToString(result.Comment.AuthorID)
		handler.publish(protocol.EventCommentCreated, workspaceID, actorType, actorID, map[string]any{"comment": commentToResponse(result.Comment, nil, nil)})
		handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, actorType, actorID)
		if result.Message.ID.Valid {
			message := result.Message
			handler.publishChatFor(protocol.EventChatMessage, workspaceID, actorType, actorID, uuidToString(result.Request.HumanUserID), uuidToString(message.ChatSessionID), protocol.ChatMessagePayload{ChatSessionID: uuidToString(message.ChatSessionID), MessageID: uuidToString(message.ID), Role: message.Role, Content: message.Content, CreatedAt: timestampToString(message.CreatedAt), MessageKind: message.MessageKind})
		}
	}
	writeJSON(writer, http.StatusOK, result.Request)
}

func (handler *Handler) withdrawDecisionRequestTx(request *http.Request, located db.DecisionRequest, reason string) (decisionRequestResult, error) {
	ctx := request.Context()
	empty := decisionRequestResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "only the asking agent's running task or the named human can withdraw this ask"}
	if isServerDecision(located) {
		return empty, decisionIntentRefusal{http.StatusForbidden, "the platform opened this request; it cannot be withdrawn"}
	}
	actorType := "member"
	actorID, err := util.ParseUUID(request.Header.Get("X-User-ID"))
	if err != nil {
		return empty, deny
	}
	var taskID, userID pgtype.UUID
	switch request.Header.Get("X-Actor-Source") {
	case "task_token":
		actorType = "agent"
		actorID, err = util.ParseUUID(request.Header.Get("X-Agent-ID"))
		if err != nil || actorID != located.AskingAgentID {
			return empty, deny
		}
		taskID, err = util.ParseUUID(request.Header.Get("X-Task-ID"))
		if err != nil {
			return empty, deny
		}
	case "interactive_jwt", "cli_jwt", "pat":
		userID = actorID
		if userID != located.HumanUserID {
			return empty, deny
		}
	default:
		return empty, deny
	}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, located.WorkspaceID); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: located.WorkspaceID, ID: located.ChatSessionID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision lane not found"}
	} else if err != nil {
		return empty, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, located.WorkspaceID, located.IssueCommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: located.WorkspaceID, ID: located.IssueID}); err != nil {
		return empty, err
	}
	if taskID.Valid {
		if _, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: located.WorkspaceID, ID: taskID}); err != nil {
			return empty, err
		}
		task, err := queries.GetAgentTask(ctx, taskID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (task.AgentID != actorID || task.Status != "running" || task.IssueID != located.IssueID && task.ChatSessionID != located.ChatSessionID) {
			return empty, deny
		}
		if err != nil {
			return empty, err
		}
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: located.WorkspaceID, UserID: located.HumanUserID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, deny
	} else if err != nil {
		return empty, err
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: located.WorkspaceID, ID: located.ID})
	if err != nil {
		return empty, err
	}
	if current.IssueID != located.IssueID || current.ChatSessionID != located.ChatSessionID || current.AskingAgentID != located.AskingAgentID || current.HumanUserID != located.HumanUserID {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision context changed; retry"}
	}
	if current.Status == "cancelled" && current.CancelReason.String == "agent_withdrawn" && current.WithdrawReason == reason {
		return decisionRequestResult{Request: current, Replayed: true}, nil
	}
	if current.Origin != "asked" || current.Status != "open" {
		return empty, decisionIntentRefusal{http.StatusConflict, "only an open unanswered ask can be withdrawn"}
	}
	parentID := pgtype.UUID{}
	if root != nil {
		parentID = current.IssueCommentID
	}
	comment, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: dbid.NewV7(), WorkspaceID: current.WorkspaceID, IssueID: current.IssueID, ParentID: parentID, AuthorType: actorType, AuthorID: actorID, SourceTaskID: taskID, Type: "comment", Content: "Decision withdrawn: " + reason, DecisionRequestID: current.ID, DecisionCommentKind: pgtype.Text{String: "cancel", Valid: true}})
	if err != nil {
		return empty, err
	}
	current, err = queries.WithdrawOpenDecisionRequest(ctx, db.WithdrawOpenDecisionRequestParams{WorkspaceID: current.WorkspaceID, ID: current.ID, Reason: reason, TaskID: taskID, UserID: userID})
	if err != nil {
		return empty, err
	}
	if _, err := decisiontx.EndAction(ctx, queries, current, "agent_withdrawn"); err != nil {
		return empty, err
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: current.WorkspaceID, ID: current.ChatSessionID})
	if err != nil {
		return empty, err
	}
	var message db.ChatMessage
	if session.Status == "active" {
		message, err = queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: "Decision withdrawn: " + reason + "\n\nOriginal ask:\n" + current.Ask, MessageKind: pgtype.Text{String: protocol.ChatMessageKindDecisionCancelNotice, Valid: true}, SentVia: pgtype.Text{String: "task_token", Valid: true}})
		if err != nil {
			return empty, err
		}
		if err := queries.StampDecisionCancelMessageRequests(ctx, db.StampDecisionCancelMessageRequestsParams{ID: message.ID, ChatSessionID: session.ID, RequestIds: []pgtype.UUID{current.ID}}); err != nil {
			return empty, err
		}
		if err := queries.TouchChatSession(ctx, session.ID); err != nil {
			return empty, err
		}
	}
	var cleared []db.Comment
	if root != nil {
		cleared, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, WorkspaceID: current.WorkspaceID, IssueID: current.IssueID})
		if err != nil {
			return empty, err
		}
	}
	if _, err := queries.MarkInboxReadByDecisionRequest(ctx, db.MarkInboxReadByDecisionRequestParams{WorkspaceID: current.WorkspaceID, DecisionRequestID: uuidToString(current.ID)}); err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return decisionRequestResult{Request: current, Comment: comment.Comment(), Message: message, ClearedComments: cleared}, nil
}
