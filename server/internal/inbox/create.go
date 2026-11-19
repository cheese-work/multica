package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CreateIfEligible returns inserted=false without an error when the recipient
// is excluded by delivery preferences or does not belong to the workspace.
// Other database errors remain failures.
// Workspace membership is checked by the insert's snapshot, without additional locks.
func CreateIfEligible(ctx context.Context, queries *db.Queries, params db.CreateInboxItemParams) (item db.InboxItem, inserted bool, err error) {
	return createIfEligible(ctx, queries, params, true)
}

// createIfEligible returns inserted=false without an error when the recipient
// does not belong to the workspace. Other database errors remain failures.
// Eligibility is checked by the insert's snapshot, without additional locks.
//
// A row written on an issue the recipient has snoozed joins the snooze (see
// CreateInboxItem). When the row is one that needs the recipient
// (WakesSnooze), the snooze ends instead and the issue is back in the inbox
// with this row on top. Announce the returned row with NewItemEventType.
//
// Fork: the unassigned_activity mute (AllowsDelivery) is checked here, so both
// CreateIfEligible and upstream's Deliver honor it (CHE-6566 A3c review).
// groupMute also applies the type's preference group (CHE-6398). Deliver
// passes false: it checked the group itself, with IgnorePreferences and a
// fanout's preloaded preferences.
func createIfEligible(ctx context.Context, queries *db.Queries, params db.CreateInboxItemParams, groupMute bool) (item db.InboxItem, inserted bool, err error) {
	if params.RecipientType == "member" {
		itemType := ""
		if groupMute {
			itemType = params.Type
		}
		allowed, err := AllowsDelivery(ctx, queries, params.WorkspaceID, params.RecipientID, params.IssueID, itemType, isDecision(params.Details))
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
	if item.SnoozedUntil.Valid && WakesSnooze(item.Type, item.Details) {
		if _, err := queries.UnsnoozeInboxGroup(ctx, db.UnsnoozeInboxGroupParams{
			WorkspaceID:   item.WorkspaceID,
			RecipientType: item.RecipientType,
			RecipientID:   item.RecipientID,
			IssueID:       item.IssueID,
		}); err != nil {
			// The row is written; it stays snoozed with the rest of the issue.
			slog.Warn("inbox: failed to end snooze for a waking notification",
				"type", item.Type, "error", err)
			return item, true, nil
		}
		item.SnoozedUntil = pgtype.Timestamptz{}
		item.SnoozedAt = pgtype.Timestamptz{}
	}
	return item, true, nil
}

// AllowsDelivery reports whether a member's preferences let an item reach
// them. A decision bypasses every preference. Otherwise the
// unassigned_activity scope switch runs first, then itemType's group mute;
// an empty itemType skips the group check.
func AllowsDelivery(ctx context.Context, queries *db.Queries, workspaceID, recipientID, issueID pgtype.UUID, itemType string, decision bool) (bool, error) {
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
	if itemType != "" && Muted(preferences, itemType) {
		return false, nil
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
	// it ignores notification preferences like a decision does. So does the
	// notice that a delegated task ran out of retries (CHE-6398).
	return json.Unmarshal(details, &decision) == nil && (decision.RequestID.Valid || decision.Kind == "run_budget" || decision.Kind == "delegated_failure_exhausted" || (decision.Kind == "decision_expiry_digest" && len(decision.RequestIDs) > 0))
}
