package decisiontx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// NotifyPing records the ping in its own transaction; the worker and the
// legacy request path use it after the request has committed.
func NotifyPing(ctx context.Context, starter interface {
	Begin(context.Context) (pgx.Tx, error)
}, queries *db.Queries, workspaceID, requestID pgtype.UUID) (*db.InboxItem, error) {
	return notifyPing(ctx, starter, queries, workspaceID, requestID, false)
}

// RepingCoalesced sends the one ping for a coalesced ask whose pinged sibling
// was answered or cancelled (CHE-2689). The worker calls it for each
// ListDecisionPingRepings row.
func RepingCoalesced(ctx context.Context, starter interface {
	Begin(context.Context) (pgx.Tx, error)
}, queries *db.Queries, workspaceID, requestID pgtype.UUID) (*db.InboxItem, error) {
	return notifyPing(ctx, starter, queries, workspaceID, requestID, true)
}

func notifyPing(ctx context.Context, starter interface {
	Begin(context.Context) (pgx.Tx, error)
}, queries *db.Queries, workspaceID, requestID pgtype.UUID, reping bool) (*db.InboxItem, error) {
	transaction, err := starter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback(ctx)
	item, err := notifyPingTx(ctx, queries.WithTx(transaction), workspaceID, requestID, reping)
	if err != nil {
		return nil, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, err
	}
	return item, nil
}

// NotifyPingTx creates the ping inbox item and its receipt inside the caller's
// transaction, so a request path can commit the request and its ping together.
func NotifyPingTx(ctx context.Context, locked *db.Queries, workspaceID, requestID pgtype.UUID) (*db.InboxItem, error) {
	return notifyPingTx(ctx, locked, workspaceID, requestID, false)
}

// notifyPingTx also coalesces: when the same agent already has an open ask to
// the same person in the same DM with an unread ping item, this request gets
// no new item. That item's line becomes "N open asks from <agent>" and this
// request's receipt is written as 'coalesced', so the worker retry skips it.
// reping re-pings a coalesced request once its pinged sibling is gone.
func notifyPingTx(ctx context.Context, locked *db.Queries, workspaceID, requestID pgtype.UUID, reping bool) (*db.InboxItem, error) {
	located, err := locked.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := locked.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return nil, err
	}
	if _, err := locked.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: located.IssueID}); err != nil {
		return nil, err
	}
	request, err := locked.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if request.Origin != "asked" || (request.PingNotifiedAt.Valid && !(reping && request.Status == "open" && request.PingOutcome.String == "coalesced")) {
		return nil, nil
	}
	var item *db.InboxItem
	outcome := "notified"
	if request.Status != "cancelled" {
		group := db.GetDecisionPingHeadItemParams{WorkspaceID: workspaceID, AskingAgentID: request.AskingAgentID, HumanUserID: request.HumanUserID, ChatSessionID: request.ChatSessionID, ID: request.ID}
		if err := locked.LockDecisionPingGroup(ctx, db.LockDecisionPingGroupParams{AskingAgentID: request.AskingAgentID, HumanUserID: request.HumanUserID, ChatSessionID: request.ChatSessionID}); err != nil {
			return nil, err
		}
		head, err := locked.GetDecisionPingHeadItem(ctx, group)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			if reping {
				return nil, nil // another ask's ping is live again; this one stays covered
			}
			count, err := locked.CountDecisionPingGroupAsks(ctx, db.CountDecisionPingGroupAsksParams{WorkspaceID: workspaceID, AskingAgentID: request.AskingAgentID, HumanUserID: request.HumanUserID, ChatSessionID: request.ChatSessionID, ID: request.ID})
			if err != nil {
				return nil, err
			}
			agent, err := locked.GetAgent(ctx, request.AskingAgentID)
			if err != nil {
				return nil, err
			}
			if err := locked.UpdateDecisionPingLine(ctx, db.UpdateDecisionPingLineParams{WorkspaceID: workspaceID, ID: head.ID, Body: pgtype.Text{String: fmt.Sprintf("%d open asks from %s", count, agent.Name), Valid: true}, OpenAskCount: count}); err != nil {
				return nil, err
			}
			if _, err := locked.MarkDecisionPingNotified(ctx, db.MarkDecisionPingNotifiedParams{WorkspaceID: workspaceID, ID: requestID, Outcome: pgtype.Text{String: "coalesced", Valid: true}}); err != nil {
				return nil, err
			}
			return nil, nil
		}
		issue, err := locked.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: request.IssueID})
		if err != nil {
			return nil, err
		}
		details, err := json.Marshal(map[string]pgtype.UUID{"comment_id": request.IssueCommentID, "decision_request_id": request.ID})
		if err != nil {
			return nil, err
		}
		body := request.Ask
		if reping {
			count, err := locked.CountDecisionPingGroupAsks(ctx, db.CountDecisionPingGroupAsksParams{WorkspaceID: workspaceID, AskingAgentID: request.AskingAgentID, HumanUserID: request.HumanUserID, ChatSessionID: request.ChatSessionID, ID: request.ID})
			if err != nil {
				return nil, err
			}
			if count > 1 {
				agent, err := locked.GetAgent(ctx, request.AskingAgentID)
				if err != nil {
					return nil, err
				}
				body = fmt.Sprintf("%d open asks from %s", count, agent.Name)
			}
		}
		// A recipient who left the workspace gets no item; the ping is still
		// recorded as handled below so the worker does not retry it.
		created, inserted, err := inbox.CreateIfEligible(ctx, locked, db.CreateInboxItemParams{
			ID: dbid.NewV7(), WorkspaceID: workspaceID, RecipientType: "member", RecipientID: request.HumanUserID,
			Type: "mentioned", Severity: "info", IssueID: request.IssueID, Title: issue.Title,
			Body: pgtype.Text{String: body, Valid: true}, ActorType: pgtype.Text{String: "agent", Valid: true},
			ActorID: request.AskingAgentID, Details: details,
			// The ask lives in a DM; the item opens it.
			ChatSessionID: request.ChatSessionID, ChatMessageID: request.PingMessageID,
		})
		if err != nil {
			return nil, err
		}
		if inserted {
			item = &created
		}
	}
	if _, err := locked.MarkDecisionPingNotified(ctx, db.MarkDecisionPingNotifiedParams{WorkspaceID: workspaceID, ID: requestID, Outcome: pgtype.Text{String: outcome, Valid: true}}); err != nil {
		return nil, err
	}
	return item, nil
}
