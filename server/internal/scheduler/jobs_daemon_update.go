package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Job names and scope kind are audit keys; keep them stable (CHE-5708).
const (
	JobNameDaemonUpdateFire   = "daemon_update_fire"
	JobNameDaemonUpdateCycles = "daemon_update_cycles"
	// ScopeKindDaemonUpdateMachine: one scope per machine with an enabled
	// schedule. scope_id is "<owner uuid>/<daemon id>".
	ScopeKindDaemonUpdateMachine = "daemon_update_machine"
)

// maxDaemonUpdateLateness: a cron time older than this at lease time is skipped,
// never fired late (plan decision D7). A machine that misses its window waits
// for the next one.
const maxDaemonUpdateLateness = 2 * time.Minute

// DaemonUpdateCycler is the part of the handler these jobs call. It lives in
// package handler; the interface keeps the scheduler free of that import.
type DaemonUpdateCycler interface {
	FireUpdateCycle(ctx context.Context, ownerID pgtype.UUID, daemonID string, fireAt time.Time, window time.Duration) (string, error)
	AdvanceUpdateCycles(ctx context.Context, now time.Time) (int, error)
}

// DaemonUpdateFireJob leases one execution per (machine, cron time) in the
// schedule's own time zone, so two server instances never fire the same time
// twice. Missed times collapse to the latest one and a stale one is skipped.
func DaemonUpdateFireJob(pool *pgxpool.Pool, queries *db.Queries, cycler DaemonUpdateCycler) JobSpec {
	cache := newDaemonUpdateScheduleCache()
	_ = pool
	return JobSpec{
		Name: JobNameDaemonUpdateFire, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: time.Minute, StaleTimeout: 3 * time.Minute, HeartbeatInterval: 20 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 3,
		RetryBackoff:    []time.Duration{20 * time.Second, 40 * time.Second},
		MaxPlansPerTick: 4, // 2 min of lateness holds at most 3 times of an every-minute cron
		Scopes: func(ctx context.Context, _ time.Time) ([]Scope, error) {
			rows, err := queries.ListEnabledDaemonUpdateSchedules(ctx)
			if err != nil {
				return nil, fmt.Errorf("daemon update scopes: %w", err)
			}
			next := make(map[string]db.DaemonUpdateSchedule, len(rows))
			scopes := make([]Scope, 0, len(rows))
			for _, r := range rows {
				id := daemonUpdateScopeID(r.MachineOwnerID, r.DaemonID)
				next[id] = r
				scopes = append(scopes, Scope{Kind: ScopeKindDaemonUpdateMachine, ID: id})
			}
			cache.replace(next)
			return scopes, nil
		},
		PlansForScope: func(_ context.Context, scope Scope, now time.Time, latest LatestPlanInfo) ([]time.Time, error) {
			sched, ok := cache.get(scope.ID)
			if !ok {
				return nil, nil
			}
			// Never replay a time from before the schedule was last saved.
			after := sched.UpdatedAt.Time.UTC()
			if latest.Found && latest.PlanTime.After(after) {
				after = latest.PlanTime
			}
			if floor := now.Add(-maxDaemonUpdateLateness); after.Before(floor) {
				after = floor.Add(-time.Nanosecond)
			}
			occs, err := service.NextOccurrencesUTC(sched.Cron, sched.Timezone, after, now)
			if err != nil {
				return nil, err
			}
			// A retry-eligible FAILED plan must come back unchanged, or its
			// lease is stranded (same rule as the autopilot planner); newer
			// eligible times follow it. Every eligible time is returned, oldest
			// first: a delayed tick must not lose the earlier ones. A later one
			// that finds a cycle still open ends `skipped_open_cycle`.
			if latest.RetryEligible(now) && now.Sub(latest.PlanTime) <= maxDaemonUpdateLateness {
				occs = append([]time.Time{latest.PlanTime}, occs...)
			}
			return occs, nil
		},
		Handler: func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
			// The lease can come late (a retry, a slow tick): re-check the age at
			// the moment of the lease, not only at planning time.
			if time.Since(in.PlanTime) > maxDaemonUpdateLateness {
				return HandlerResult{Result: map[string]any{"skipped_reason": "stale_plan"}}, nil
			}
			sched, ok := cache.get(in.Scope.ID)
			if !ok {
				return HandlerResult{Result: map[string]any{"skipped_reason": "schedule_gone"}}, nil
			}
			state, err := cycler.FireUpdateCycle(ctx, sched.MachineOwnerID, sched.DaemonID, in.PlanTime, time.Duration(sched.WindowMinutes)*time.Minute)
			if err != nil {
				return HandlerResult{}, fmt.Errorf("fire update cycle: %w", err)
			}
			return HandlerResult{RowsAffected: 1, Result: map[string]any{"state": state}}, nil
		},
	}
}

// DaemonUpdateCyclesJob advances every draining cycle every 30 s under one
// global lease. The per-cycle work is conditional on (id, state), so a missed or
// repeated tick is safe.
func DaemonUpdateCyclesJob(cycler DaemonUpdateCycler) JobSpec {
	return JobSpec{
		Name: JobNameDaemonUpdateCycles, Cadence: 30 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			n, err := cycler.AdvanceUpdateCycles(ctx, time.Now())
			return HandlerResult{RowsAffected: int64(n)}, err
		},
	}
}

func daemonUpdateScopeID(owner pgtype.UUID, daemonID string) string {
	return util.UUIDToString(owner) + "/" + daemonID
}

type daemonUpdateScheduleCache struct {
	mu sync.RWMutex
	m  map[string]db.DaemonUpdateSchedule
}

func newDaemonUpdateScheduleCache() *daemonUpdateScheduleCache {
	return &daemonUpdateScheduleCache{m: map[string]db.DaemonUpdateSchedule{}}
}

func (c *daemonUpdateScheduleCache) replace(next map[string]db.DaemonUpdateSchedule) {
	c.mu.Lock()
	c.m = next
	c.mu.Unlock()
}

func (c *daemonUpdateScheduleCache) get(id string) (db.DaemonUpdateSchedule, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[id]
	return v, ok
}
