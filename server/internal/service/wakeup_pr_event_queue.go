package service

import (
	"encoding/json"
	"errors"
	"fmt"

	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Next-turn fallback for PR events (CHE-2298 a3). An event the mid-run channel
// could not deliver (no capability, a claim that lost the race with turn end)
// is stored as next_turn. The scheduler hands each one to the issue's
// pr_needs_attention rule as a receipt: the rule keeps at most one pending
// wake, and the receipts it holds are the causes that wake lists.
const (
	wakeupPREventType    = "pr.event"
	wakeupPREventKeyBase = "pr_event:"
	wakeupPREventBatch   = 100
)

// queueNextTurnPREvents turns next_turn events into receipts, once each.
// Events stay stored: queued here, delivered when a run takes them along.
func (s *IssueWakeupService) queueNextTurnPREvents(ctx context.Context, workspaceIDs []pgtype.UUID) error {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	events, err := q.ListNextTurnPREvents(ctx, db.ListNextTurnPREventsParams{WorkspaceIds: workspaceIDs, RowLimit: wakeupPREventBatch})
	if err != nil || len(events) == 0 {
		return err
	}
	for _, e := range events {
		w, err := q.CreateSystemWakeup(ctx, db.CreateSystemWakeupParams{
			ID: dbid.NewV7(), WorkspaceID: e.WorkspaceID, IssueID: e.IssueID,
			EventTypes: []string{}, Enabled: true, SystemRule: systemRuleText(SystemRulePRAttention),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			w, err = q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: e.IssueID, SystemRule: systemRuleText(SystemRulePRAttention)})
		}
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"event_id": util.UUIDToString(e.ID), "kind": e.Kind, "pr_number": e.PrNumber,
			"repo_owner": e.RepoOwner, "repo_name": e.RepoName, "head_sha": e.HeadSha, "base_sha": e.BaseSha,
		})
		if err != nil {
			return err
		}
		if _, err := q.RecordWakeupReceipt(ctx, db.RecordWakeupReceiptParams{
			ID: dbid.NewV7(), WakeupID: w.ID, Revision: w.Revision,
			EventKey: wakeupPREventKeyBase + util.UUIDToString(e.ID), EventType: wakeupPREventType, Payload: payload,
		}); err != nil {
			return err
		}
		if err := q.MarkPREventQueued(ctx, e.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// prEventCause names one pending PR fact for the "queued for next turn" activity.
func prEventCause(r db.IssueWakeupReceipt) string {
	var p struct {
		Kind     string `json:"kind"`
		PRNumber int32  `json:"pr_number"`
	}
	_ = json.Unmarshal(r.Payload, &p)
	if p.Kind == "" {
		p.Kind = r.EventType
	}
	return fmt.Sprintf("%s on PR #%d", p.Kind, p.PRNumber)
}
