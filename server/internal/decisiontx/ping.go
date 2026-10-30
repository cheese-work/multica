package decisiontx

import (
	"context"
	"encoding/json"
	"errors"

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
	transaction, err := starter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback(ctx)
	item, err := NotifyPingTx(ctx, queries.WithTx(transaction), workspaceID, requestID)
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
	if request.PingNotifiedAt.Valid || request.Origin != "asked" {
		return nil, nil
	}
	var item *db.InboxItem
	if request.Status != "cancelled" {
		issue, err := locked.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: request.IssueID})
		if err != nil {
			return nil, err
		}
		details, err := json.Marshal(map[string]pgtype.UUID{"comment_id": request.IssueCommentID, "decision_request_id": request.ID})
		if err != nil {
			return nil, err
		}
		// A recipient who left the workspace gets no item; the ping is still
		// recorded as handled below so the worker does not retry it.
		created, inserted, err := inbox.CreateIfEligible(ctx, locked, db.CreateInboxItemParams{
			ID: dbid.NewV7(), WorkspaceID: workspaceID, RecipientType: "member", RecipientID: request.HumanUserID,
			Type: "mentioned", Severity: "info", IssueID: request.IssueID, Title: issue.Title,
			Body: pgtype.Text{String: request.Ask, Valid: true}, ActorType: pgtype.Text{String: "agent", Valid: true},
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
	if _, err := locked.MarkDecisionPingNotified(ctx, db.MarkDecisionPingNotifiedParams{WorkspaceID: workspaceID, ID: requestID}); err != nil {
		return nil, err
	}
	return item, nil
}
