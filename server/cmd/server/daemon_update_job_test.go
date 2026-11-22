package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/scheduler"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Scheduler-level checks for the daemon_update_fire job (CHE-5708): one lease per
// (machine, cron time), the cron read in the schedule's time zone, and a stale
// cron time skipped (plan decision D7). The cycle logic is tested in package handler.

type fireRecord struct {
	fireAt time.Time
	window time.Duration
}

type recordingCycler struct {
	mu    sync.Mutex
	fired map[string][]fireRecord
}

func (r *recordingCycler) FireUpdateCycle(_ context.Context, _ pgtype.UUID, daemonID string, fireAt time.Time, window time.Duration) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fired == nil {
		r.fired = map[string][]fireRecord{}
	}
	r.fired[daemonID] = append(r.fired[daemonID], fireRecord{fireAt, window})
	return "draining", nil
}

func (r *recordingCycler) AdvanceUpdateCycles(context.Context, time.Time) (int, error) { return 0, nil }

func (r *recordingCycler) firesOf(daemonID string) []fireRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]fireRecord(nil), r.fired[daemonID]...)
}

// setupDaemonUpdateFire saves an enabled schedule for a new machine, saved an
// hour ago so only times inside the lateness window are due, and returns two
// managers (two "server instances") with the fire job registered.
func setupDaemonUpdateFire(t *testing.T, cron, tz string) (string, *recordingCycler, *scheduler.Manager, *scheduler.Manager) {
	t.Helper()
	ctx := context.Background()
	daemonID := fmt.Sprintf("che5708-job-%d", time.Now().UnixNano())
	owner := parseUUID(testUserID)
	if _, err := db.New(testPool).UpsertDaemonUpdateSchedule(ctx, db.UpsertDaemonUpdateScheduleParams{
		MachineOwnerID: owner, DaemonID: daemonID, Enabled: true, Cron: cron, Timezone: tz, WindowMinutes: 20,
	}); err != nil {
		t.Fatalf("save schedule: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE daemon_update_schedule SET updated_at = now() - interval '1 hour' WHERE daemon_id = $1`, daemonID); err != nil {
		t.Fatalf("backdate schedule: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		testPool.Exec(bg, `DELETE FROM daemon_update_schedule WHERE daemon_id = $1`, daemonID)
		testPool.Exec(bg, `DELETE FROM sys_cron_executions WHERE job_name = $1 AND scope_id LIKE '%/' || $2`, scheduler.JobNameDaemonUpdateFire, daemonID)
	})
	cycler := &recordingCycler{}
	var mgrs [2]*scheduler.Manager
	for i := range mgrs {
		mgrs[i] = scheduler.NewManager(testPool, scheduler.Options{RunnerID: fmt.Sprintf("daemon-update-job-test-%d", i)})
		if err := mgrs[i].Register(scheduler.DaemonUpdateFireJob(testPool, db.New(testPool), cycler)); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	return daemonID, cycler, mgrs[0], mgrs[1]
}

func TestDaemonUpdateFireJob_OneLeasePerMachineCronTime(t *testing.T) {
	ctx := context.Background()
	daemonID, cycler, a, b := setupDaemonUpdateFire(t, "* * * * *", "UTC")
	for _, m := range []*scheduler.Manager{a, b, a, b} {
		if err := m.RunOnce(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	fires := cycler.firesOf(daemonID)
	if len(fires) == 0 {
		t.Fatal("no fire for an every-minute schedule")
	}
	seen := map[time.Time]bool{}
	for _, f := range fires {
		if seen[f.fireAt] {
			t.Fatalf("cron time %s fired twice across two instances", f.fireAt)
		}
		seen[f.fireAt] = true
		if f.window != 20*time.Minute {
			t.Fatalf("window = %s, want the schedule's 20m", f.window)
		}
	}
	var rows int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM sys_cron_executions WHERE job_name = $1 AND scope_id LIKE '%/' || $2 AND status = 'SUCCESS'`,
		scheduler.JobNameDaemonUpdateFire, daemonID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(fires) {
		t.Fatalf("SUCCESS leases = %d, fires = %d; want one lease per cron time", rows, len(fires))
	}
}

func TestDaemonUpdateFireJob_CronReadInScheduleTimeZone(t *testing.T) {
	ctx := context.Background()
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	// One minute ago on the machine's wall clock. Read as UTC the same cron
	// would be 7 hours off and never due.
	due := time.Now().Add(-time.Minute).In(loc).Truncate(time.Minute)
	daemonID, cycler, a, _ := setupDaemonUpdateFire(t, fmt.Sprintf("%d %d * * *", due.Minute(), due.Hour()), "Asia/Ho_Chi_Minh")
	if err := a.RunOnce(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	fires := cycler.firesOf(daemonID)
	if len(fires) != 1 || !fires[0].fireAt.Equal(due) {
		t.Fatalf("fires = %v, want one at %s", fires, due.UTC())
	}
}

func TestDaemonUpdateFireJob_StalePlanSkipped(t *testing.T) {
	ctx := context.Background()
	// Daily cron whose last time was 10 minutes ago: older than the 2 minute
	// lateness limit, so it is skipped, not fired late.
	old := time.Now().UTC().Add(-10 * time.Minute)
	daemonID, cycler, a, b := setupDaemonUpdateFire(t, fmt.Sprintf("%d %d * * *", old.Minute(), old.Hour()), "UTC")
	for _, m := range []*scheduler.Manager{a, b} {
		if err := m.RunOnce(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if fires := cycler.firesOf(daemonID); len(fires) != 0 {
		t.Fatalf("a 10 minute old cron time fired: %v", fires)
	}
	var rows int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM sys_cron_executions WHERE job_name = $1 AND scope_id LIKE '%/' || $2`,
		scheduler.JobNameDaemonUpdateFire, daemonID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("lease rows for a stale cron time = %d, want 0", rows)
	}
}

// A delayed tick can find several cron times inside the lateness window. Each is
// leased and fired, oldest first; only a time older than the limit is skipped
// (review of PR 1836).
func TestDaemonUpdateFireJob_EveryEligibleCronTimeFiresInOrder(t *testing.T) {
	ctx := context.Background()
	daemonID, cycler, a, _ := setupDaemonUpdateFire(t, "* * * * *", "UTC")
	if err := a.RunOnce(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	fires := cycler.firesOf(daemonID)
	// The newest cron time is at most 60 s old and the one before it at most 120 s
	// old, so an every-minute schedule always has at least two eligible times.
	if len(fires) < 2 {
		t.Fatalf("fires = %v, want every eligible cron time (at least 2)", fires)
	}
	for i := 1; i < len(fires); i++ {
		if !fires[i].fireAt.After(fires[i-1].fireAt) {
			t.Fatalf("fires out of order: %v", fires)
		}
	}
	if age := time.Since(fires[0].fireAt); age > 2*time.Minute+5*time.Second {
		t.Fatalf("fired a cron time %s old, over the 2 minute limit", age)
	}
}
