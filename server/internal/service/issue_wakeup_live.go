package service

import (
	"context"
	"errors"

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
	byID := make(map[pgtype.UUID]db.IssueWakeupReceipt, len(free))
	for _, r := range free {
		byID[r.ID] = r
	}
	for _, item := range items {
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
		if item.Status != "delivered" || item.TaskStatus != "completed" {
			if !out.holdRun.Valid {
				out.holdRun = item.TaskID
			}
			continue
		}
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
	}
	return out, nil
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
