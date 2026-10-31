package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (h *Handler) sendChatDecisionReply(r *http.Request, session db.ChatSession, agent db.Agent, humanID, requestID pgtype.UUID, content string, attachmentIDs []pgtype.UUID) (*service.DirectChatSendResult, error) {
	deny := decisionIntentRefusal{http.StatusForbidden, "decision replies require the request's human in its original interactive DM"}
	if r.Header.Get("X-Actor-Source") != "interactive_jwt" || session.CreatorID != humanID || agent.Kind == "system" {
		return nil, deny
	}
	if service.IsCompactCommand(content) {
		return nil, decisionIntentRefusal{http.StatusBadRequest, "/compact cannot answer a decision request"}
	}
	if strings.TrimSpace(content) == "" || len(blockQuote(content)) > maxCommentContentBytes {
		return nil, decisionIntentRefusal{http.StatusBadRequest, "decision reply is empty or too long"}
	}
	ctx := r.Context()
	plan, err := h.TaskService.PlanDirectChatSend(ctx, session.ID, agent, humanID)
	if err != nil {
		return nil, err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	queries := h.Queries.WithTx(tx)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, session.WorkspaceID); err != nil {
		return nil, err
	}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: session.WorkspaceID, ID: session.ID}); err != nil {
		return nil, err
	}
	session, err = queries.GetChatSession(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	if session.CreatorID != humanID || session.Status != "active" || session.ChannelOriginAt.Valid || session.IsAgentIntro {
		return nil, deny
	}
	if _, err := queries.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); err == nil {
		return nil, deny
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: session.WorkspaceID, UserID: humanID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, deny
	} else if err != nil {
		return nil, err
	}
	decision, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: session.WorkspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, decisionIntentRefusal{http.StatusNotFound, "decision request not found"}
	} else if err != nil {
		return nil, err
	}
	if decision.ChatSessionID != session.ID || decision.HumanUserID != humanID {
		return nil, deny
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, session.WorkspaceID, decision.IssueCommentID)
	if err != nil {
		return nil, err
	}
	if root == nil || root.IssueID != decision.IssueID {
		return nil, decisionIntentRefusal{http.StatusConflict, "decision ask is unavailable"}
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: session.WorkspaceID, ID: decision.IssueID}); err != nil {
		return nil, err
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: session.WorkspaceID, ID: decision.IssueID})
	if err != nil {
		return nil, err
	}
	// An ask by an agent other than the DM's steward is answered task-less, and
	// only in a topic DM. The ask was made in this DM (checked above), and its
	// topic was fixed then; do not recompute it from the issue's current place.
	foreign := decision.AskingAgentID != session.AgentID
	if foreign {
		if !session.DecisionTopicID.Valid {
			return nil, deny
		}
		if len(attachmentIDs) > 0 {
			return nil, decisionIntentRefusal{http.StatusBadRequest, "a reply to another agent's request takes no attachments"}
		}
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil {
		return nil, err
	}
	if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return nil, decisionIntentRefusal{http.StatusConflict, "decision issue is closed"}
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: decision.ID})
	if err != nil {
		return nil, err
	}
	if current.ChatSessionID != decision.ChatSessionID || current.HumanUserID != decision.HumanUserID || current.AskingAgentID != decision.AskingAgentID || current.IssueID != decision.IssueID || current.IssueCommentID != decision.IssueCommentID || current.Status != "open" || current.Origin != "asked" {
		return nil, decisionIntentRefusal{http.StatusConflict, "this request cannot accept another answer"}
	}
	var sent service.DirectChatSendResult
	if foreign {
		message, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "user", Content: content, MessageKind: pgtype.Text{String: "message", Valid: true}, SentVia: pgtype.Text{String: "interactive_jwt", Valid: true}})
		if err != nil {
			return nil, err
		}
		if err := queries.TouchChatSession(ctx, session.ID); err != nil {
			return nil, err
		}
		sent.Message = message
	} else if sent, err = h.TaskService.SendDirectChatMessageTx(ctx, queries, plan, session, humanID, content, attachmentIDs, "member", humanID, "interactive_jwt"); err != nil {
		return nil, err
	}
	result, err := h.persistDecisionRecord(ctx, queries, issue, current, decisionRecordInput{
		ParentID: current.IssueCommentID, MessageIDs: []pgtype.UUID{sent.Message.ID},
	}, decisionRecordSource{
		TaskID: sent.Task.ID, AgentID: decision.AskingAgentID, SessionID: session.ID, SourceSessionID: session.ID,
		CommentID: dbid.NewV7(), Snapshot: content, Quote: blockQuote(content), ReplyText: content,
	}, root)
	if err != nil {
		return nil, err
	}
	if _, err := queries.BindChatMessageDecisionAnswer(ctx, db.BindChatMessageDecisionAnswerParams{
		WorkspaceID: session.WorkspaceID, SessionID: session.ID, MessageID: sent.Message.ID, AnswerID: result.Answer.ID,
	}); err != nil {
		return nil, err
	}
	sent.Message.DecisionAnswerID = result.Answer.ID
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if !foreign {
		h.TaskService.RecordHumanDelegation(ctx, plan.Attribution)
		h.TaskService.NotifyDirectChatSent(ctx, sent.Task)
	}
	h.DecisionWorker.Notify()
	workspaceID, agentID := uuidToString(issue.WorkspaceID), uuidToString(decision.AskingAgentID)
	response := commentToResponse(result.Comment, nil, nil)
	response.DecisionCommentFields = h.decisionCommentFields(ctx, issue.WorkspaceID, []db.Comment{result.Comment}, pgtype.UUID{})[response.ID]
	h.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{"comment": response, "issue_title": issue.Title, "issue_status": issue.Status})
	h.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "agent", agentID)
	h.publishDecisionAnswerUpdated(workspaceID, "agent", agentID, result.Answer.ID, issue.ID, decisionAnswerState(result.Answer.WokenAt, result.Answer.DisclaimedAt))
	h.publishChatFor(protocol.EventChatMessage, workspaceID, "agent", agentID, uuidToString(session.CreatorID), uuidToString(session.ID), protocol.ChatMessagePayload{
		ChatSessionID: uuidToString(session.ID), MessageID: uuidToString(result.Message.ID), Role: result.Message.Role, Content: result.Message.Content, CreatedAt: timestampToString(result.Message.CreatedAt),
	})
	return &sent, nil
}
