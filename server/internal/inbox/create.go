package inbox

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CreateIfEligible returns inserted=false without an error when the recipient
// is excluded by delivery preferences or does not belong to the workspace.
// Other database errors remain failures.
// Workspace membership is checked by the insert's snapshot, without additional locks.
func CreateIfEligible(ctx context.Context, queries *db.Queries, params db.CreateInboxItemParams) (item db.InboxItem, inserted bool, err error) {
	if params.RecipientType == "member" {
		allowed, err := AllowsDelivery(ctx, queries, params.WorkspaceID, params.RecipientID, params.IssueID, isDecision(params.Details))
		if err != nil || !allowed {
			return db.InboxItem{}, false, err
		}
	}
	item, err = queries.CreateInboxItem(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.InboxItem{}, false, nil
	}
	if err != nil {
		return db.InboxItem{}, false, err
	}
	return item, true, nil
}

func AllowsDelivery(ctx context.Context, queries *db.Queries, workspaceID, recipientID, issueID pgtype.UUID, decision bool) (bool, error) {
	if decision {
		return true, nil
	}
	preference, err := queries.GetNotificationPreference(ctx, db.GetNotificationPreferenceParams{WorkspaceID: workspaceID, UserID: recipientID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var preferences map[string]string
	if err := json.Unmarshal(preference.Preferences, &preferences); err != nil {
		return false, err
	}
	if preferences["unassigned_activity"] != "muted" {
		return true, nil
	}
	if !issueID.Valid {
		return false, nil
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: issueID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return issue.AssigneeType.String == "member" && issue.AssigneeID.Valid && issue.AssigneeID == recipientID, nil
}

func isDecision(details []byte) bool {
	var decision struct {
		RequestID  pgtype.UUID   `json:"decision_request_id"`
		Kind       string        `json:"kind"`
		RequestIDs []pgtype.UUID `json:"decision_request_ids"`
	}
	// A run_budget item is the one signal that work stopped (CHE-2297 L1a), so
	// it ignores notification preferences like a decision does.
	return json.Unmarshal(details, &decision) == nil && (decision.RequestID.Valid || decision.Kind == "run_budget" || (decision.Kind == "decision_expiry_digest" && len(decision.RequestIDs) > 0))
}
