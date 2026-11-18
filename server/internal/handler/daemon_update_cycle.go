package handler

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// updateStartCutoff is how long before deadline_at the last update may start
// (plan rule 1); a draining cycle that reaches it ends no_window.
const updateStartCutoff = 3 * time.Minute

// FireUpdateCycle handles one cron time of one machine's update schedule
// (CHE-5708, plan rules 1 to 3). It returns the state the cron time ended in or
// reached: `up_to_date`, `draining`, `skipped_manual_pause`, `skipped_open_cycle`,
// or `no_runtime` when the machine has no runtime to update. It is idempotent on
// (machine, fireAt): a repeat returns the existing cycle's state.
func (h *Handler) FireUpdateCycle(ctx context.Context, ownerID pgtype.UUID, daemonID string, fireAt time.Time, window time.Duration) (string, error) {
	return h.fireUpdateCycle(ctx, ownerID, daemonID, fireAt, window, nil)
}

// fireUpdateCycle is FireUpdateCycle with the OpenCycle test hook.
func (h *Handler) fireUpdateCycle(ctx context.Context, ownerID pgtype.UUID, daemonID string, fireAt time.Time, window time.Duration, afterInsert func() error) (string, error) {
	fireTS := pgtype.Timestamptz{Time: fireAt, Valid: true}
	if existing, err := h.Queries.GetDaemonUpdateCycleByFire(ctx, db.GetDaemonUpdateCycleByFireParams{MachineOwnerID: ownerID, DaemonID: daemonID, FireAt: fireTS}); err == nil {
		return existing.State, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	runtimes, err := h.Queries.ListMachineRuntimes(ctx, db.ListMachineRuntimesParams{OwnerID: ownerID, DaemonID: pgtype.Text{String: daemonID, Valid: true}})
	if err != nil || len(runtimes) == 0 {
		return "no_runtime", err
	}

	// Same verdict as the update_available badge; a machine with no newer build
	// is recorded and never paused (D4).
	needsUpdate := false
	for _, rt := range runtimes {
		needsUpdate = needsUpdate || h.DaemonUpdate.runtimeUpdateStatus(rt.Metadata).UpdateAvailable
	}
	if !needsUpdate {
		now := time.Now().Truncate(time.Microsecond)
		c, err := h.Queries.InsertSkippedDaemonUpdateCycle(ctx, db.InsertSkippedDaemonUpdateCycleParams{
			ID: dbid.NewV7(), MachineOwnerID: ownerID, DaemonID: daemonID, FireAt: fireTS,
			StartedAt:  pgtype.Timestamptz{Time: now, Valid: true},
			DeadlineAt: pgtype.Timestamptz{Time: now.Add(window), Valid: true},
			Outcome:    "up_to_date",
		})
		return c.State, err
	}

	c, err := h.openCycle(ctx, CycleMachine{OwnerID: ownerID, DaemonID: daemonID, WorkspaceID: runtimes[0].WorkspaceID}, fireAt, window, afterInsert)
	return c.State, err
}

// AdvanceUpdateCycles takes one driver step on every open `draining` cycle and
// returns how many it ended or handed to the update step. Each step is one
// transaction under the machine lock with a conditional update on (id, state), so
// two drivers, or a restart, never double-step a cycle (plan rule 7). `now` is a
// parameter so a test can move a cycle past its cutoff.
func (h *Handler) AdvanceUpdateCycles(ctx context.Context, now time.Time) (int, error) {
	cycles, err := h.Queries.ListDrainingDaemonUpdateCycles(ctx)
	if err != nil {
		return 0, err
	}
	var errs []error
	advanced := 0
	for _, c := range cycles {
		ok, err := h.stepDrainingCycle(ctx, c, now)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			advanced++
		}
	}
	return advanced, errors.Join(errs...)
}

// stepDrainingCycle applies the first matching step of the plan's driver order:
// cancel requested, start cutoff, pause lost, then ready to update.
func (h *Handler) stepDrainingCycle(ctx context.Context, listed db.DaemonUpdateCycle, now time.Time) (bool, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockMachine(ctx, tx, listed.MachineOwnerID, listed.DaemonID); err != nil {
		return false, err
	}
	qtx := h.Queries.WithTx(tx)
	c, err := qtx.LockDaemonUpdateCycle(ctx, listed.ID)
	if err != nil || c.State != "draining" || c.FinishedAt.Valid {
		return false, err
	}

	finish := func(outcome, detail string, liveRuns *int32) (bool, error) {
		if _, err := qtx.ReleaseCyclePause(ctx, db.ReleaseCyclePauseParams{MachineOwnerID: c.MachineOwnerID, DaemonID: c.DaemonID, CycleID: c.ID}); err != nil {
			return false, err
		}
		if _, err := qtx.FinishDaemonUpdateCycle(ctx, db.FinishDaemonUpdateCycleParams{
			ID: c.ID, FromState: "draining", Outcome: outcome,
			OutcomeDetail: pgtype.Text{String: detail, Valid: detail != ""},
			LiveRunsAtEnd: pgtype.Int4{Int32: derefInt32(liveRuns), Valid: liveRuns != nil},
		}); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		PublishMachineRefresh(ctx, h.Queries, h.Bus, c.MachineOwnerID, c.DaemonID, pgtype.UUID{}, "", "system", "", "update_cycle")
		return true, nil
	}

	// Runs and runtimes are read once; steps 2 and 4 use them.
	liveRuns, online, err := h.machineLoad(ctx, qtx, c)
	if err != nil {
		return false, err
	}
	switch {
	case c.CancelRequested.Valid:
		return finish("cancelled", c.CancelRequested.String, nil)
	case !now.Before(c.DeadlineAt.Time.Add(-updateStartCutoff)):
		detail := ""
		if !online {
			detail = "offline"
		}
		return finish("no_window", detail, &liveRuns)
	}
	holds, err := qtx.CycleHoldsLivePause(ctx, db.CycleHoldsLivePauseParams{
		MachineOwnerID: c.MachineOwnerID, DaemonID: c.DaemonID, CycleID: c.ID, Now: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return false, err
	}
	if !holds {
		return finish("cancelled", "pause_lost", nil)
	}
	if liveRuns > 0 || !online || h.UpdateCycleReady == nil {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, h.UpdateCycleReady(ctx, c)
}

// machineLoad is the machine's live run count (server side) and whether any of
// its runtimes is online.
func (h *Handler) machineLoad(ctx context.Context, q *db.Queries, c db.DaemonUpdateCycle) (liveRuns int32, online bool, err error) {
	counts, err := q.CountRunningTasksByMachine(ctx, db.CountRunningTasksByMachineParams{
		MachineOwnerIds: []pgtype.UUID{c.MachineOwnerID}, DaemonIds: []string{c.DaemonID},
	})
	if err != nil {
		return 0, false, err
	}
	for _, row := range counts {
		if row.DaemonID == c.DaemonID && util.UUIDToString(row.MachineOwnerID) == util.UUIDToString(c.MachineOwnerID) {
			liveRuns += row.RunningTasks
		}
	}
	runtimes, err := q.ListMachineRuntimes(ctx, db.ListMachineRuntimesParams{OwnerID: c.MachineOwnerID, DaemonID: pgtype.Text{String: c.DaemonID, Valid: true}})
	for _, rt := range runtimes {
		online = online || rt.Status == "online"
	}
	return liveRuns, online, err
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}
