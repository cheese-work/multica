package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// deliverDecisionReminder is the one reminder of an episode (CHE-2591 L5
// design §4.6). One transaction inserts the inbox item, the lane message and
// the reminder_delivered_at stamp, so a failure at any point leaves nothing and
// a retry or a second worker cannot add a second reminder. It never creates a
// request, a lane or a DM, and it leaves the card answerable.
func (handler *Handler) deliverDecisionReminder(ctx context.Context, workspaceID, waitID pgtype.UUID) error {
	located, err := handler.Queries.GetDecisionWait(ctx, db.GetDecisionWaitParams{WorkspaceID: workspaceID, ID: waitID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	requests, err := handler.Queries.ListDecisionWaitRequests(ctx, db.ListDecisionWaitRequestsParams{WorkspaceID: workspaceID, RequestIds: located.RequestIds})
	if err != nil {
		return err
	}
	bound, ok := reminderRequest(located, requests)
	if !ok {
		return nil
	}

	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	// Lane, issue, request, then the episode: the veto notice's order.
	var session db.ChatSession
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: workspaceID, ID: bound.ChatSessionID}); err == nil {
		if session, err = queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: workspaceID, ID: bound.ChatSessionID}); err != nil {
			return err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: located.IssueID}); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: workspaceID, UserID: located.HumanUserID}); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	request, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: bound.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	wait, err := queries.LockDecisionWait(ctx, db.LockDecisionWaitParams{WorkspaceID: workspaceID, ID: waitID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Re-check under the locks: the episode may have closed, suspended, been
	// reminded by another worker, or lost the open request or the person.
	if wait.ClosedAt.Valid || wait.Phase != "waiting" || wait.ReminderDeliveredAt.Valid ||
		request.Status != "open" || request.HumanUserID != wait.HumanUserID || request.ChatSessionID != bound.ChatSessionID ||
		!slices.Contains(wait.RequestIds, request.ID) {
		return nil
	}

	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: request.IssueID})
	if err != nil {
		return err
	}
	workspace, err := queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	content := fmt.Sprintf("Reminder: [%s-%d](mention://issue/%s) is still waiting for your decision: %s\nOpen issue: %s",
		workspace.IssuePrefix, issue.Number, uuidToString(issue.ID), decisionAskPreview(request.Ask), util.WorkspaceIssuePath(workspace.Slug, uuidToString(issue.ID)))
	var message db.ChatMessage
	if session.ID.Valid && session.Status == "active" {
		message, err = queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: content, MessageKind: pgtype.Text{String: "decision_reminder", Valid: true}, SentVia: pgtype.Text{String: "system", Valid: true}})
		if err != nil {
			return err
		}
		if err := queries.TouchChatSession(ctx, session.ID); err != nil {
			return err
		}
	}
	details, err := json.Marshal(map[string]any{"kind": "decision_wait_reminder", "decision_request_id": request.ID, "comment_id": request.IssueCommentID})
	if err != nil {
		return err
	}
	item, inserted, err := inbox.CreateIfEligible(ctx, queries, db.CreateInboxItemParams{ID: dbid.NewV7(), WorkspaceID: workspaceID, RecipientType: "member", RecipientID: request.HumanUserID, Type: "mentioned", Severity: "info", IssueID: issue.ID, Title: issue.Title, Body: pgtype.Text{String: content, Valid: true}, ActorType: pgtype.Text{String: "agent", Valid: true}, ActorID: wait.AskingAgentID, Details: details})
	if err != nil {
		return err
	}
	if rows, err := queries.MarkDecisionReminderDelivered(ctx, db.MarkDecisionReminderDeliveredParams{WorkspaceID: workspaceID, ID: wait.ID}); err != nil {
		return err
	} else if rows != 1 {
		return errors.New("decision reminder stamp changed; retry")
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}

	workspaceText, agentID := uuidToString(workspaceID), uuidToString(wait.AskingAgentID)
	if message.ID.Valid {
		handler.publishChatFor(protocol.EventChatMessage, workspaceText, "agent", agentID, uuidToString(request.HumanUserID), uuidToString(message.ChatSessionID), protocol.ChatMessagePayload{ChatSessionID: uuidToString(message.ChatSessionID), MessageID: uuidToString(message.ID), Role: message.Role, Content: message.Content, CreatedAt: timestampToString(message.CreatedAt), MessageKind: message.MessageKind})
	}
	if inserted {
		handler.publish(protocol.EventInboxNew, workspaceText, "agent", agentID, map[string]any{"item": inboxToResponse(item)})
	}
	return nil
}

// reminderRequest is the request the reminder points at: the first open one of
// the episode that is still R's.
func reminderRequest(wait db.DecisionWait, requests []db.DecisionRequest) (db.DecisionRequest, bool) {
	for _, request := range requests {
		if request.Status == "open" && request.HumanUserID == wait.HumanUserID {
			return request, true
		}
	}
	return db.DecisionRequest{}, false
}
