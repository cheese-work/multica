package handler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The receipt lives in issue_idle_guard, never in issue.metadata (CHE-3947):
// metadata holds only primitive values, and an object there stops the issue
// from opening in the client.

func readIdleGuardReceipt(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID) (idleGuardReceipt, bool, error) {
	row, err := q.GetIssueIdleGuard(ctx, db.GetIssueIdleGuardParams{WorkspaceID: workspaceID, IssueID: issueID})
	if errors.Is(err, pgx.ErrNoRows) {
		return idleGuardReceipt{}, false, nil
	}
	if err != nil {
		return idleGuardReceipt{}, false, err
	}
	return idleGuardReceiptFromRow(row), true, nil
}

// idleGuardNotifiedKey is the one primitive left in issue.metadata: the status
// the guard moved the issue to and told its leader about. The continuation
// watchdog (multica-dotfiles) reads it to leave such an issue alone.
const idleGuardNotifiedKey = "idle_guard_notified_to"

func writeIdleGuardReceipt(ctx context.Context, q *db.Queries, issue db.Issue, receipt idleGuardReceipt) error {
	alertID, _ := parseUUIDLoose(receipt.AlertID)
	commentID, _ := parseUUIDLoose(receipt.CommentID)
	if err := q.UpsertIssueIdleGuard(ctx, db.UpsertIssueIdleGuardParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, State: receipt.State, Kind: receipt.Kind,
		FromStatus: receipt.From, ToStatus: receipt.To,
		IdleSince: idleGuardTime(receipt.IdleSince), MovedAt: idleGuardTime(receipt.MovedAt),
		Attempts: int32(receipt.Attempts), AlertID: alertID, CommentID: commentID, Error: receipt.Error,
		AlertAttempts: int32(receipt.AlertAttempts), AlertAt: idleGuardTime(receipt.AlertAt), AlertError: receipt.AlertError,
	}); err != nil {
		return err
	}
	if receipt.State != idleGuardNotified || receipt.To == "" {
		return clearIdleGuardNotified(ctx, q, issue)
	}
	raw, err := json.Marshal(receipt.To)
	if err != nil {
		return err
	}
	_, err = q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: idleGuardNotifiedKey, Value: raw})
	if errors.Is(err, pgx.ErrNoRows) { // the value is already set
		return nil
	}
	return err
}

// deleteIdleGuardReceipt ends the episode: the row and the watchdog signal go together.
func deleteIdleGuardReceipt(ctx context.Context, q *db.Queries, workspaceID, issueID pgtype.UUID) error {
	if err := q.DeleteIssueIdleGuard(ctx, db.DeleteIssueIdleGuardParams{WorkspaceID: workspaceID, IssueID: issueID}); err != nil {
		return err
	}
	return clearIdleGuardNotified(ctx, q, db.Issue{WorkspaceID: workspaceID, ID: issueID})
}

func clearIdleGuardNotified(ctx context.Context, q *db.Queries, issue db.Issue) error {
	_, err := q.DeleteIssueMetadataKey(ctx, db.DeleteIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: idleGuardNotifiedKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func idleGuardReceiptFromRow(row db.IssueIdleGuard) idleGuardReceipt {
	receipt := idleGuardReceipt{
		State: row.State, Kind: row.Kind, From: row.FromStatus, To: row.ToStatus,
		IdleSince: idleGuardStamp(row.IdleSince, time.RFC3339Nano), MovedAt: idleGuardStamp(row.MovedAt, time.RFC3339),
		Attempts: int(row.Attempts), Error: row.Error,
		AlertAttempts: int(row.AlertAttempts), AlertAt: idleGuardStamp(row.AlertAt, time.RFC3339Nano), AlertError: row.AlertError,
	}
	if row.AlertID.Valid {
		receipt.AlertID = uuidToString(row.AlertID)
	}
	if row.CommentID.Valid {
		receipt.CommentID = uuidToString(row.CommentID)
	}
	return receipt
}

// idleGuardStamp formats a stored time; an unset time is the empty string.
func idleGuardStamp(at pgtype.Timestamptz, layout string) string {
	if !at.Valid {
		return ""
	}
	return at.Time.UTC().Format(layout)
}

// idleGuardTime parses a receipt time; the empty string stays NULL.
func idleGuardTime(exact string) pgtype.Timestamptz {
	at, err := time.Parse(time.RFC3339Nano, exact)
	return pgtype.Timestamptz{Time: at, Valid: err == nil}
}
