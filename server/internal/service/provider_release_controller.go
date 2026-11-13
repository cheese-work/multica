package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1741 M3b: the evidence-gated release controller. It releases provider-held
// rows on health evidence, never on a timer alone, with one canary at a time.
// All state lives in provider_breaker and the held rows, so a restart resumes.
const (
	providerReleaseStaggerEvery = 15 * time.Second
	providerReleaseStaggerBatch = 2
	providerCanaryMaxCoolOff    = 1800 // seconds: 5 min doubling to 30 min
	providerCanaryPerHour       = 6
	providerReleaseScanRows     = 200 // ponytail: oldest 200 held rows per provider per tick; page if backlogs exceed it
	providerServerErrorReason   = "agent_error.provider_server_error"
)

var errProviderReleaseRaced = errors.New("provider breaker changed during release")

// ReleaseProviderHeldTasks runs one idempotent controller pass. now is a
// parameter so tests drive a fake clock. A group that cannot be read or
// released is skipped, never released on a guess.
func (s *TaskService) ReleaseProviderHeldTasks(ctx context.Context, now time.Time) error {
	groups, err := s.Queries.ListProviderHeldGroups(ctx)
	if err != nil {
		return fmt.Errorf("list provider held groups: %w", err)
	}
	var errs []error
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return err
		}
		if g.Provider == "" {
			continue
		}
		lockKey := fmt.Sprintf("%x:%s", g.WorkspaceID.Bytes, g.Provider)
		err := s.runInTx(ctx, func(q *db.Queries) error {
			if err := q.LockProviderBreakerTrip(ctx, lockKey); err != nil {
				return fmt.Errorf("lock provider breaker release: %w", err)
			}
			return releaseProviderGroup(ctx, q, g.WorkspaceID, g.Provider, now)
		})
		if err != nil {
			if errors.Is(err, errProviderReleaseRaced) {
				slog.Info("provider release skipped after concurrent breaker change", "provider", g.Provider)
				continue
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func releaseProviderGroup(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, provider string, now time.Time) error {
	row, exists, err := providerBreakerRow(ctx, q, workspaceID, provider)
	if err != nil || !exists {
		return err // no breaker row is unknown state: release nothing
	}
	ws, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("read workspace for provider hold: %w", err)
	}
	holds, err := ProviderHoldsFromSettings(ws.Settings)
	if err != nil {
		return err // fails closed
	}
	if _, held := findProviderHold(holds, provider); held {
		return nil
	}
	rows, err := q.ListProviderHeldRows(ctx, db.ListProviderHeldRowsParams{
		WorkspaceID: workspaceID, Provider: provider, Now: releaseTS(now), MaxRows: providerReleaseScanRows,
	})
	if err != nil {
		return fmt.Errorf("list provider held rows: %w", err)
	}
	switch row.State {
	case "closed":
		return releaseClosed(ctx, q, row, rows, now)
	case "open", "half_open":
		return releaseProbe(ctx, q, row, rows, now)
	case "draining":
		return releaseDraining(ctx, q, row, rows, now)
	}
	return nil
}

func releaseTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// releaseRow lifts the hold of one row. live is false when the row was
// cancelled as wakeup_superseded (not a start) or was no longer held.
func releaseRow(ctx context.Context, q *db.Queries, row db.GetProviderBreakerRow, id pgtype.UUID, now time.Time) (live bool, err error) {
	t, err := q.ReleaseProviderHeldRow(ctx, db.ReleaseProviderHeldRowParams{Now: releaseTS(now), WorkspaceID: row.WorkspaceID, TaskID: id, Provider: row.Provider})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("release held row: %w", err)
	}
	return t.Status != "cancelled", nil
}

// canaryEnded reports whether the canary slot is free. A task that is still
// queued, deferred or running holds it; a missing task is free.
func canaryEnded(ctx context.Context, q *db.Queries, id pgtype.UUID) (task db.AgentTaskQueue, ended bool, err error) {
	if !id.Valid {
		return task, true, nil
	}
	task, err = q.GetAgentTask(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return task, true, nil
	}
	if err != nil {
		return task, false, fmt.Errorf("read canary task: %w", err)
	}
	switch task.Status {
	case "completed", "failed", "cancelled":
		return task, true, nil
	}
	return task, false, nil
}

// pickCanary prefers a non-recovery row; only when none exists is the oldest
// recovery retry the canary (D5). rows are oldest first.
func pickCanary(rows []db.ListProviderHeldRowsRow) []db.ListProviderHeldRowsRow {
	out := make([]db.ListProviderHeldRowsRow, 0, len(rows))
	for _, r := range rows {
		if !r.IsRecovery {
			out = append(out, r)
		}
	}
	for _, r := range rows {
		if r.IsRecovery {
			out = append(out, r)
		}
	}
	return out
}

// hasEvidence: a success newer than the row's failure. A row with no failed
// task has nothing to compare, so any recorded success counts.
func hasEvidence(row db.GetProviderBreakerRow, r db.ListProviderHeldRowsRow) bool {
	return row.LastSuccessAt.Valid && (!r.FailedAt.Valid || row.LastSuccessAt.Time.After(r.FailedAt.Time))
}

func staggerOpen(row db.GetProviderBreakerRow, now time.Time) bool {
	return !row.LastReleaseAt.Valid || now.Sub(row.LastReleaseAt.Time) >= providerReleaseStaggerEvery
}

func releaseClosed(ctx context.Context, q *db.Queries, row db.GetProviderBreakerRow, rows []db.ListProviderHeldRowsRow, now time.Time) error {
	save := db.SaveProviderReleaseStateParams{
		WorkspaceID: row.WorkspaceID, Provider: row.Provider, ExpectedVersion: row.Version,
		CanaryTaskID: row.CanaryTaskID, CanaryCoolOffS: row.CanaryCoolOffS, NextCanaryAt: row.NextCanaryAt,
		CanaryStarts: row.CanaryStarts, LastReleaseAt: row.LastReleaseAt,
	}
	dirty := false

	canary, ended, err := canaryEnded(ctx, q, row.CanaryTaskID)
	if err != nil {
		return err
	}
	if row.CanaryTaskID.Valid && ended {
		switch {
		case canary.Status == "failed" && canary.FailureReason.String == providerServerErrorReason:
			// A failed canary leaves the evidence missing and doubles the cool-off.
			save.CanaryCoolOffS = min(row.CanaryCoolOffS*2, providerCanaryMaxCoolOff)
			save.NextCanaryAt = releaseTS(canary.CompletedAt.Time.Add(time.Duration(save.CanaryCoolOffS) * time.Second))
		case canary.Status == "completed":
			save.CanaryCoolOffS, save.NextCanaryAt = providerBreakerCoolOff, pgtype.Timestamptz{}
		}
		save.CanaryTaskID, dirty = pgtype.UUID{}, true
	}
	busy := save.CanaryTaskID.Valid

	var evidenced []db.ListProviderHeldRowsRow
	for _, r := range rows {
		if hasEvidence(row, r) {
			evidenced = append(evidenced, r)
		}
	}
	switch {
	case len(evidenced) > 0: // R1: staggered release
		if staggerOpen(row, now) {
			released := 0
			for _, r := range evidenced {
				if released == providerReleaseStaggerBatch {
					break
				}
				live, err := releaseRow(ctx, q, row, r.ID, now)
				if err != nil {
					return err
				}
				if live {
					released++
				}
			}
			if released > 0 {
				save.LastReleaseAt, dirty = releaseTS(now), true
			}
		}
	case !busy && len(rows) > 0 && (!save.NextCanaryAt.Valid || !save.NextCanaryAt.Time.After(now)): // R2: one canary
		var recent []pgtype.Timestamptz
		for _, st := range row.CanaryStarts {
			if st.Time.After(now.Add(-time.Hour)) {
				recent = append(recent, st)
			}
		}
		save.CanaryStarts = recent
		dirty = dirty || len(recent) != len(row.CanaryStarts)
		if len(recent) >= providerCanaryPerHour {
			break
		}
		for _, r := range pickCanary(rows) {
			live, err := releaseRow(ctx, q, row, r.ID, now)
			if err != nil {
				return err
			}
			if live {
				save.CanaryTaskID, save.CanaryStarts = r.ID, append(recent, releaseTS(now))
				save.NextCanaryAt, dirty = releaseTS(now.Add(time.Duration(save.CanaryCoolOffS)*time.Second)), true
				break
			}
		}
	}
	if !dirty {
		return nil
	}
	if n, err := q.SaveProviderReleaseState(ctx, save); err != nil {
		return fmt.Errorf("save provider release state: %w", err)
	} else if n != 1 {
		return errProviderReleaseRaced
	}
	return nil
}

// releaseProbe is R3: at next_probe_at the breaker goes half_open on one canary.
// A half_open breaker whose canary ended without a verdict probes again.
func releaseProbe(ctx context.Context, q *db.Queries, row db.GetProviderBreakerRow, rows []db.ListProviderHeldRowsRow, now time.Time) error {
	if !row.ProbeDue {
		return nil
	}
	if _, ended, err := canaryEnded(ctx, q, row.CanaryTaskID); err != nil || !ended {
		return err
	}
	for _, r := range pickCanary(rows) {
		live, err := releaseRow(ctx, q, row, r.ID, now)
		if err != nil {
			return err
		}
		if !live {
			continue
		}
		n, err := q.BeginProviderBreakerProbe(ctx, db.BeginProviderBreakerProbeParams{
			WorkspaceID: row.WorkspaceID, Provider: row.Provider, TaskID: r.ID, ExpectedVersion: row.Version,
		})
		if err != nil {
			return fmt.Errorf("begin provider breaker probe: %w", err)
		}
		if n != 1 {
			return errProviderReleaseRaced
		}
		return recordProviderBreakerTransition(ctx, q, row.WorkspaceID, row.Provider, "half_open", row.State, "half_open", pgtype.UUID{}, r.ID)
	}
	return nil
}

// releaseDraining releases the backlog staggered, then closes. Rows held after
// the drain began stay held (M3c) and are released by the closed path.
func releaseDraining(ctx context.Context, q *db.Queries, row db.GetProviderBreakerRow, rows []db.ListProviderHeldRowsRow, now time.Time) error {
	var backlog []db.ListProviderHeldRowsRow
	for _, r := range rows {
		if !row.DrainCutoffAt.Valid || !r.Since.Time.After(row.DrainCutoffAt.Time) {
			backlog = append(backlog, r)
		}
	}
	if len(backlog) == 0 {
		n, err := q.CloseDrainedProviderBreaker(ctx, db.CloseDrainedProviderBreakerParams{
			WorkspaceID: row.WorkspaceID, Provider: row.Provider, DefaultCoolOffSeconds: providerBreakerCoolOff, ExpectedVersion: row.Version,
		})
		if err != nil {
			return fmt.Errorf("close drained provider breaker: %w", err)
		}
		if n != 1 {
			return errProviderReleaseRaced
		}
		return recordProviderBreakerTransition(ctx, q, row.WorkspaceID, row.Provider, "close", "draining", "closed", pgtype.UUID{}, pgtype.UUID{})
	}
	if !staggerOpen(row, now) {
		return nil
	}
	released := 0
	for _, r := range backlog {
		if released == providerReleaseStaggerBatch {
			break
		}
		live, err := releaseRow(ctx, q, row, r.ID, now)
		if err != nil {
			return err
		}
		if live {
			released++
		}
	}
	if released == 0 {
		return nil
	}
	n, err := q.MarkProviderReleaseTick(ctx, db.MarkProviderReleaseTickParams{
		WorkspaceID: row.WorkspaceID, Provider: row.Provider, Now: releaseTS(now), ExpectedVersion: row.Version,
	})
	if err != nil {
		return fmt.Errorf("mark provider release tick: %w", err)
	}
	if n != 1 {
		return errProviderReleaseRaced
	}
	return nil
}
