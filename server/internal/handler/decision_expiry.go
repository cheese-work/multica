package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const decisionExpiryExplanation = "Expired after 7 days. Expiry is not an answer or permission to execute."

const decisionActionExpiryExplanation = "The approval window for this action closed. Expiry is not an answer or permission to execute."

func (handler *Handler) deliverDecisionExpiryDigest(ctx context.Context, workspaceID, humanID pgtype.UUID) error {
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return err
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: workspaceID, UserID: humanID}); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	requests, err := queries.ListDecisionExpiryDigestRequests(ctx, db.ListDecisionExpiryDigestRequestsParams{WorkspaceID: workspaceID, HumanUserID: humanID})
	if err != nil || len(requests) == 0 {
		return err
	}
	now, err := queries.DecisionExpiryClock(ctx)
	if err != nil {
		return err
	}
	digest, err := queries.ClaimDecisionExpiryDigest(ctx, db.ClaimDecisionExpiryDigestParams{ID: dbid.NewV7(), WorkspaceID: workspaceID, HumanUserID: humanID, AsOf: now})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	workspace, err := queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	requestIDs := make([]pgtype.UUID, len(requests))
	var body strings.Builder
	body.WriteString(decisionExpiryExplanation)
	for index, request := range requests {
		issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: request.IssueID})
		if err != nil {
			return err
		}
		requestIDs[index] = request.ID
		identifier := fmt.Sprintf("%s-%d", issuePrefixForWorkspace(workspace), issue.Number)
		link := channel.IssueWebLink(resolveFrontendAppURL(), workspace.Slug, identifier)
		fmt.Fprintf(&body, "\n- %s: %s", identifier, decisionAskPreview(request.Ask))
		if link != "" {
			fmt.Fprintf(&body, "\n  %s#comment-%s", link, uuidToString(request.IssueCommentID))
		}
	}
	details, err := json.Marshal(map[string]any{"kind": "decision_expiry_digest", "decision_request_ids": requestIDs, "digest_day": digest.DigestDay.Time.Format("2006-01-02")})
	if err != nil {
		return err
	}
	item, inserted, err := inbox.CreateIfEligible(ctx, queries, db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: workspaceID, RecipientType: "member", RecipientID: humanID,
		Type: "mentioned", Severity: "info", Title: "Expired decision requests",
		Body: pgtype.Text{String: body.String(), Valid: true}, Details: details,
	})
	if err != nil || !inserted {
		return err
	}
	if err := queries.StampDecisionExpiryDigestInbox(ctx, db.StampDecisionExpiryDigestInboxParams{ID: digest.ID, InboxItemID: item.ID}); err != nil {
		return err
	}
	if err := queries.StampDecisionExpiryDigestRequests(ctx, db.StampDecisionExpiryDigestRequestsParams{DigestID: digest.ID, RequestIds: requestIDs}); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	handler.publish(protocol.EventInboxNew, uuidToString(workspaceID), "system", "", map[string]any{"item": inboxToResponse(item)})
	return nil
}
