package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A condition wakeup can hand its instruction to a run of its agent that is
// already running, as a wakeup item on the CHE-2298 delivery table (CHE-6971).
// The item's receipts are not reserved: they stay pending with no task_id, so
// takenReceipts cannot settle them by itself. This file decides what an item
// means for a dispatch pass.

const wakeupOutcomeJoinedLive = "joined_live"

// Fallback cutoffs of the disposition table. AdmitWakeupDeliveryFallback
// repeats both in its WHERE clause and stays the authority; these only spare a
// write on passes that cannot admit yet.
const (
	wakeupUnclaimedAfter = 120 * time.Second // a live run claims within one 5 s poll
	wakeupLateAckGrace   = 60 * time.Second  // an ack can still land after turn_ended
)

// fallbackAdmission is an item this pass sent to fallback. Its receipts stay in
// the free list and take today's path in the same transaction.
type fallbackAdmission struct {
	item   db.ListWakeupDeliveryItemsRow
	reason string
	// failureReason is the host run's failure_reason for a host-run end, or the
	// item's own for a provider rejection.
	failureReason string
}

// liveItems is what a pass learns from the wakeup's items.
type liveItems struct {
	// holdRun is the host run of an open item with a pending receipt; Valid
	// means the pass holds.
	holdRun pgtype.UUID
	// settled are delivered items whose host run completed. Their receipts are
	// consumed with that run, as if it had started with them.
	settled []takenRun
	// settledReceipts are the receipts of settled, to drop from the free list.
	settledReceipts map[pgtype.UUID]bool
	// fallbacks are items admitted to fallback in this pass.
	fallbacks []fallbackAdmission
}

// readLiveItems sorts the wakeup's items. free are the pending, unreserved
// receipts this pass locked; an item counts only through the receipts of its
// own that are still among them, so a discarded or consumed receipt never
// holds or counts.
func readLiveItems(ctx context.Context, q *db.Queries, w db.IssueWakeup, free []db.IssueWakeupReceipt) (liveItems, error) {
	var out liveItems
	items, err := q.ListWakeupDeliveryItems(ctx, w.ID)
	if err != nil || len(items) == 0 {
		return out, err
	}
	// A host run that ended by a path that skipped terminal settlement leaves
	// its pending and delivering items open. Settle them first, then decide.
	if settled, err := settleEndedHostItems(ctx, q, items); err != nil {
		return out, err
	} else if settled {
		if items, err = q.ListWakeupDeliveryItems(ctx, w.ID); err != nil {
			return out, err
		}
	}
	byID := make(map[pgtype.UUID]db.IssueWakeupReceipt, len(free))
	for _, r := range free {
		byID[r.ID] = r
	}
	for _, listed := range items {
		item := listed
		if item.Status == "fallback" {
			continue
		}
		var mine []db.IssueWakeupReceipt
		for _, id := range item.ReceiptIds {
			if r, ok := byID[id]; ok {
				mine = append(mine, r)
			}
		}
		if len(mine) == 0 {
			continue
		}
		if item.Status == "delivered" && item.TaskStatus == "completed" {
			host, err := q.GetAgentTask(ctx, item.TaskID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return out, err
			}
			if out.settledReceipts == nil {
				out.settledReceipts = map[pgtype.UUID]bool{}
			}
			for _, r := range mine {
				out.settledReceipts[r.ID] = true
			}
			out.settled = append(out.settled, takenRun{task: host, receipts: mine, outcome: wakeupOutcomeJoinedLive, firingID: item.EventID})
			continue
		}
		reason := fallbackReason(item)
		if reason != "" {
			// No row means the ack won (or the cutoff is not reached): hold.
			_, err := q.AdmitWakeupDeliveryFallback(ctx, db.AdmitWakeupDeliveryFallbackParams{
				WakeupID: w.ID, EventID: item.EventID, SeenStatus: item.Status, Reason: reason})
			if err == nil {
				fb := fallbackAdmission{item: item, reason: reason}
				switch reason {
				case "host_cancelled", "host_failed":
					fb.failureReason = item.TaskFailureReason.String
				case "rejected":
					fb.failureReason = item.FailureReason.String
				}
				out.fallbacks = append(out.fallbacks, fb)
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return out, err
			}
		}
		if !out.holdRun.Valid {
			out.holdRun = item.TaskID
		}
	}
	return out, nil
}

// settleEndedHostItems applies terminal settlement to the host runs of open
// items whose run already ended. It reports whether any run needed it.
func settleEndedHostItems(ctx context.Context, q *db.Queries, items []db.ListWakeupDeliveryItemsRow) (bool, error) {
	var ended []pgtype.UUID
	for _, item := range items {
		if (item.Status == "pending" || item.Status == "delivering") && hostEnded(item.TaskStatus) {
			ended = append(ended, item.TaskID)
		}
	}
	if len(ended) == 0 {
		return false, nil
	}
	_, err := q.SettleTerminalTaskPREventDeliveries(ctx, ended)
	return err == nil, err
}

func hostEnded(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// fallbackReason is the disposition table's admission reason for an item, or
// "" when the pass holds. A candidate is only a request: the admission update
// repeats the full condition and decides.
func fallbackReason(item db.ListWakeupDeliveryItemsRow) string {
	hostStatus := item.TaskStatus
	switch item.Status {
	case "pending":
		if !hostEnded(hostStatus) && time.Since(item.CreatedAt.Time) >= wakeupUnclaimedAfter {
			return "unclaimed"
		}
	case "failed":
		switch {
		case item.FailureReason.String != "turn_ended":
			return "rejected"
		case item.AttemptCount == 0:
			return "turn_ended"
		case time.Since(item.UpdatedAt.Time) >= wakeupLateAckGrace:
			return "late_ack_cutoff"
		}
	case "delivered":
		switch hostStatus {
		case "cancelled":
			return "host_cancelled"
		case "failed":
			return "host_failed"
		}
	}
	return ""
}

// withoutSettled returns receipts minus the ones a settled item consumes.
func (l liveItems) withoutSettled(receipts []db.IssueWakeupReceipt) []db.IssueWakeupReceipt {
	if len(l.settledReceipts) == 0 {
		return receipts
	}
	rest := make([]db.IssueWakeupReceipt, 0, len(receipts))
	for _, r := range receipts {
		if !l.settledReceipts[r.ID] {
			rest = append(rest, r)
		}
	}
	return rest
}

func liveHoldReason(host pgtype.UUID) pgtype.Text {
	return pgtype.Text{String: "Delivering into running run " + util.UUIDToString(host) + "; inputs retained.", Valid: true}
}

// receiptsInLiveItems are the receipts of the wakeup's items that are not
// fallback. A run being claimed never reserves them: the item's ack settles
// the firing, and a reservation would count it a second time.
func receiptsInLiveItems(ctx context.Context, q *db.Queries, wakeupID pgtype.UUID) (map[pgtype.UUID]bool, error) {
	items, err := q.ListWakeupDeliveryItems(ctx, wakeupID)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	held := map[pgtype.UUID]bool{}
	for _, item := range items {
		if item.Status == "fallback" {
			continue
		}
		for _, id := range item.ReceiptIds {
			held[id] = true
		}
	}
	return held, nil
}
