package service

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// otherCarrierSlots counts the firings of a rule that runs other than self
// hold: a run that took some of its inputs and has not ended, or that started
// and is waiting for the rule's own dispatch to settle it. Each run is one
// firing however many inputs it took. At start only the runs ahead of self
// count, with a started run ahead of any that has not started and ties broken
// by run id, so concurrent starts agree on who gets the last slot.
func otherCarrierSlots(ctx context.Context, q *db.Queries, w db.IssueWakeup, self pgtype.UUID, atStart bool) (int32, error) {
	pending, err := q.ListPendingWakeupReceipts(ctx, db.ListPendingWakeupReceiptsParams{WakeupID: w.ID, Revision: w.Revision})
	if err != nil {
		return 0, err
	}
	seen := map[pgtype.UUID]bool{}
	var slots int32
	for _, r := range pending {
		if !r.TaskID.Valid || r.TaskID == self || seen[r.TaskID] {
			continue
		}
		seen[r.TaskID] = true
		task, err := q.GetAgentTask(ctx, r.TaskID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		started := task.StartedAt.Valid || task.Status == "running"
		ended := task.Status == "completed" || task.Status == "failed" || task.Status == "cancelled"
		if ended && !started {
			continue
		}
		if atStart && !started && bytes.Compare(task.ID.Bytes[:], self.Bytes[:]) > 0 {
			continue
		}
		slots++
	}
	return slots, nil
}

// checkJoinedFireCap holds a carrier back from starting when the rule's
// once/max_fires limit is already spoken for by the firings settled so far and
// the carriers ahead of it. It reads under the instance lock, so a settlement
// cannot move between the count and the slots.
func (s *IssueWakeupService) checkJoinedFireCap(ctx context.Context, task db.AgentTaskQueue, entry joinedWakeup, limit int32) error {
	id, err := wakeupUUID(entry.WakeupID)
	if err != nil {
		return nil
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	w, err := q.LockIssueWakeup(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if w.DisabledAt.Valid || w.Revision != entry.Revision || systemWakeupPaused(w) {
		return ErrWakeupForbidden
	}
	ahead, err := otherCarrierSlots(ctx, q, w, task.ID, true)
	if err != nil {
		return err
	}
	if w.FireCount+ahead >= limit {
		return ErrWakeupForbidden
	}
	return nil
}
