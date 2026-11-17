package handler

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Fire scheduled update cycles and drain the machine (CHE-5708). The driver
// takes `now` as an argument, so a fake clock moves a cycle past its cutoff.

type fireFixture struct{ cycleFixture }

// newFireFixture is a machine with one online runtime on the given build. The
// server's build is serverBuildCommit.
func newFireFixture(t *testing.T, daemonID, runtimeCommit string) fireFixture {
	t.Helper()
	f := fireFixture{newCycleFixture(t, daemonID)}
	dbfx.Exec(t, `UPDATE agent_runtime SET metadata = $2::jsonb WHERE id = $1`, f.runtimeID, string(runtimeMeta(runtimeCommit, "linux", "amd64")))
	prev := testHandler.DaemonUpdate
	testHandler.DaemonUpdate = testUpdateSource(serverBuildCommit)
	t.Cleanup(func() { testHandler.DaemonUpdate = prev })
	return f
}

func (f fireFixture) fire(t *testing.T, fireAt time.Time) string {
	t.Helper()
	out, err := testHandler.FireUpdateCycle(context.Background(), f.machine.OwnerID, f.machine.DaemonID, fireAt, 20*time.Minute)
	if err != nil {
		t.Fatalf("FireUpdateCycle: %v", err)
	}
	return out
}

// onlyCycle is the machine's one cycle row.
func (f fireFixture) onlyCycle(t *testing.T) db.DaemonUpdateCycle {
	t.Helper()
	if n := f.cycleCount(t); n != 1 {
		t.Fatalf("cycle rows = %d, want 1", n)
	}
	var id [16]byte
	_ = id
	var c db.DaemonUpdateCycle
	rows, err := testPool.Query(context.Background(), `SELECT id FROM daemon_update_cycle WHERE daemon_id = $1`, f.machine.DaemonID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no cycle row")
	}
	if err := rows.Scan(&c.ID); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	return f.cycle(t, c.ID)
}

// setLiveRuns leaves exactly n running tasks on the machine.
func (f fireFixture) setLiveRuns(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE runtime_id = $1`, f.runtimeID)
	for i := 0; i < n; i++ {
		agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, f.runtimeID, fmt.Sprintf("Fire run agent %s %d", f.machine.DaemonID, i))
		var id string
		if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at)
			VALUES ($1, $2, $3, 'running', 0, now()) RETURNING id`, agentID, f.runtimeID, issueID).Scan(&id); err != nil {
			t.Fatalf("seed running task: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, id) })
	}
}

// readyCalls counts the ready_to_update hook (child 9 replaces it).
func readyCalls(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := testHandler.UpdateCycleReady
	testHandler.UpdateCycleReady = func(context.Context, db.DaemonUpdateCycle) error { n.Add(1); return nil }
	t.Cleanup(func() { testHandler.UpdateCycleReady = prev })
	return &n
}

func advance(t *testing.T, now time.Time) int {
	t.Helper()
	n, err := testHandler.AdvanceUpdateCycles(context.Background(), now)
	if err != nil {
		t.Fatalf("AdvanceUpdateCycles: %v", err)
	}
	return n
}

// 1. No newer build: recorded up_to_date, no pause row.
func TestFireUpdateCycle_UpToDateWritesNoPause(t *testing.T) {
	f := newFireFixture(t, "fire-up-to-date", serverBuildCommit)
	if got := f.fire(t, time.Now()); got != "up_to_date" {
		t.Fatalf("fire = %q, want up_to_date", got)
	}
	c := f.onlyCycle(t)
	if c.Outcome.String != "up_to_date" || !c.FinishedAt.Valid || c.OutcomeSource.String != "server" {
		t.Fatalf("cycle = %+v, want a finished up_to_date from the server", c)
	}
	if p := f.pauseRow(t); p != nil {
		t.Fatalf("pause = %+v, want none", p)
	}
}

// 2. A live manual pause is left alone.
func TestFireUpdateCycle_ManualPauseUnchanged(t *testing.T) {
	f := newFireFixture(t, "fire-manual-pause", olderBuildCommit)
	pauseDaemonForTest(t, f.machine.DaemonID)
	before := f.pauseRow(t)
	if got := f.fire(t, time.Now()); got != "skipped_manual_pause" {
		t.Fatalf("fire = %q, want skipped_manual_pause", got)
	}
	after := f.pauseRow(t)
	if after == nil || after.Reason != "maintenance" || after.CycleID.Valid || !after.PausedAt.Time.Equal(before.PausedAt.Time) {
		t.Fatalf("manual pause changed: before %+v after %+v", before, after)
	}
}

// 3. Runs reach 0 only after the start cutoff: no_window, no update.
func TestAdvanceUpdateCycles_LateDrainIsNoWindow(t *testing.T) {
	f := newFireFixture(t, "fire-late-drain", olderBuildCommit)
	ready := readyCalls(t)
	f.setLiveRuns(t, 1)
	if got := f.fire(t, time.Now()); got != "draining" {
		t.Fatalf("fire = %q, want draining", got)
	}
	c := f.onlyCycle(t)
	deadline := c.DeadlineAt.Time

	if n := advance(t, deadline.Add(-10*time.Minute)); n != 0 {
		t.Fatalf("a busy machine before the cutoff advanced %d cycles, want 0", n)
	}
	f.setLiveRuns(t, 0)
	// 2 minutes before the deadline is past the 3-minute start cutoff.
	if n := advance(t, deadline.Add(-2*time.Minute)); n != 1 {
		t.Fatalf("advanced %d cycles after the cutoff, want 1", n)
	}
	c = f.cycle(t, c.ID)
	if c.Outcome.String != "no_window" || c.LiveRunsAtEnd.Int32 != 0 || c.UpdateRequestID.Valid {
		t.Fatalf("cycle = outcome %q live %v request %v, want no_window with no update request", c.Outcome.String, c.LiveRunsAtEnd, c.UpdateRequestID)
	}
	if got := ready.Load(); got != 0 {
		t.Fatalf("ready_to_update ran %d times after the cutoff, want 0", got)
	}
}

// 4. no_window with live runs: the pause is gone, the runs stay, the count shows.
func TestAdvanceUpdateCycles_NoWindowKeepsRuns(t *testing.T) {
	f := newFireFixture(t, "fire-no-window", olderBuildCommit)
	f.setLiveRuns(t, 2)
	f.fire(t, time.Now())
	c := f.onlyCycle(t)
	if n := advance(t, c.DeadlineAt.Time.Add(-3*time.Minute)); n != 1 {
		t.Fatalf("advanced %d cycles at the cutoff, want 1", n)
	}
	c = f.cycle(t, c.ID)
	if c.Outcome.String != "no_window" || c.LiveRunsAtEnd.Int32 != 2 || c.OutcomeDetail.Valid {
		t.Fatalf("cycle = outcome %q live %v detail %v, want no_window, 2 runs, no detail", c.Outcome.String, c.LiveRunsAtEnd, c.OutcomeDetail)
	}
	if p := f.pauseRow(t); p != nil {
		t.Fatalf("pause = %+v, want released", p)
	}
	var running int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE runtime_id = $1 AND status = 'running'`, f.runtimeID).Scan(&running); err != nil || running != 2 {
		t.Fatalf("running tasks = %d, err %v, want both still running", running, err)
	}
}

// 5. A person's pause or resume during draining cancels the cycle; their pause stays.
func TestAdvanceUpdateCycles_ManualActionCancels(t *testing.T) {
	t.Run("resume", func(t *testing.T) {
		f := newFireFixture(t, "fire-cancel-resume", olderBuildCommit)
		f.setLiveRuns(t, 1)
		f.fire(t, time.Now())
		c := f.onlyCycle(t)
		if w := deletePause(t, testUserID, f.runtimeID); w.Code != http.StatusNoContent {
			t.Fatalf("resume = %d", w.Code)
		}
		if n := advance(t, time.Now()); n != 1 {
			t.Fatalf("advanced %d, want 1", n)
		}
		c = f.cycle(t, c.ID)
		if c.Outcome.String != "cancelled" || c.OutcomeDetail.String != "manual_resume" {
			t.Fatalf("cycle = %q / %q, want cancelled / manual_resume", c.Outcome.String, c.OutcomeDetail.String)
		}
		if p := f.pauseRow(t); p != nil {
			t.Fatalf("pause after resume = %+v, want none", p)
		}
	})
	t.Run("pause", func(t *testing.T) {
		f := newFireFixture(t, "fire-cancel-pause", olderBuildCommit)
		f.setLiveRuns(t, 1)
		f.fire(t, time.Now())
		c := f.onlyCycle(t)
		if w := putPause(t, testUserID, f.runtimeID, map[string]any{"reason": "incident"}); w.Code != http.StatusOK {
			t.Fatalf("pause = %d", w.Code)
		}
		if n := advance(t, time.Now()); n != 1 {
			t.Fatalf("advanced %d, want 1", n)
		}
		c = f.cycle(t, c.ID)
		if c.Outcome.String != "cancelled" || c.OutcomeDetail.String != "manual_pause" {
			t.Fatalf("cycle = %q / %q, want cancelled / manual_pause", c.Outcome.String, c.OutcomeDetail.String)
		}
		if p := f.pauseRow(t); p == nil || p.Reason != "incident" || p.CycleID.Valid {
			t.Fatalf("pause = %+v, want the person's incident pause to stay", p)
		}
	})
	t.Run("pause row lost", func(t *testing.T) {
		f := newFireFixture(t, "fire-pause-lost", olderBuildCommit)
		f.setLiveRuns(t, 1)
		f.fire(t, time.Now())
		c := f.onlyCycle(t)
		dbfx.Exec(t, `DELETE FROM daemon_pause WHERE daemon_id = $1`, f.machine.DaemonID)
		if n := advance(t, time.Now()); n != 1 {
			t.Fatalf("advanced %d, want 1", n)
		}
		c = f.cycle(t, c.ID)
		if c.Outcome.String != "cancelled" || c.OutcomeDetail.String != "pause_lost" {
			t.Fatalf("cycle = %q / %q, want cancelled / pause_lost", c.Outcome.String, c.OutcomeDetail.String)
		}
	})
}

// 6. A new driver after a restart advances an old cycle; a cycle past its
// deadline ends no_window; the pause is not live after the deadline with no driver.
func TestAdvanceUpdateCycles_RestartRecovery(t *testing.T) {
	f := newFireFixture(t, "fire-restart", olderBuildCommit)
	f.setLiveRuns(t, 1)
	f.fire(t, time.Now())
	c := f.onlyCycle(t)

	// The pause stops holding the machine at deadline_at with no driver running.
	dbfx.Exec(t, `UPDATE daemon_pause SET expires_at = now() - interval '1 second' WHERE daemon_id = $1`, f.machine.DaemonID)
	if live, err := testHandler.Queries.ListLiveDaemonPauses(context.Background(), db.ListLiveDaemonPausesParams{
		MachineOwnerIds: []pgtype.UUID{f.machine.OwnerID}, DaemonIds: []string{f.machine.DaemonID},
		MaintenanceSecs: int32(maintenancePauseTTL.Seconds()), OtherSecs: int32(otherPauseTTL.Seconds()),
	}); err != nil || len(live) != 0 {
		t.Fatalf("live pauses after deadline = %v, err %v, want none", live, err)
	}

	// A fresh driver, well past the deadline, ends the cycle.
	if n := advance(t, c.DeadlineAt.Time.Add(time.Minute)); n != 1 {
		t.Fatalf("advanced %d, want 1", n)
	}
	c = f.cycle(t, c.ID)
	if c.Outcome.String != "no_window" || c.LiveRunsAtEnd.Int32 != 1 {
		t.Fatalf("cycle = %q live %v, want no_window with 1 run", c.Outcome.String, c.LiveRunsAtEnd)
	}
}

// 7. Two drivers on one cycle take one step.
func TestAdvanceUpdateCycles_TwoDriversOneStep(t *testing.T) {
	f := newFireFixture(t, "fire-two-drivers", olderBuildCommit)
	f.setLiveRuns(t, 1)
	f.fire(t, time.Now())
	c := f.onlyCycle(t)
	now := c.DeadlineAt.Time.Add(-time.Minute)
	var wg sync.WaitGroup
	var total atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := testHandler.AdvanceUpdateCycles(context.Background(), now)
			if err != nil {
				t.Errorf("AdvanceUpdateCycles: %v", err)
			}
			total.Add(int32(n))
		}()
	}
	wg.Wait()
	if got := total.Load(); got != 1 {
		t.Fatalf("two drivers took %d steps, want 1", got)
	}
}

// 8. A cron time while the previous cycle is open records skipped_open_cycle;
// the open cycle continues.
func TestFireUpdateCycle_OpenCycleSkipped(t *testing.T) {
	f := newFireFixture(t, "fire-open-cycle", olderBuildCommit)
	f.setLiveRuns(t, 1)
	first := time.Now().Truncate(time.Millisecond)
	f.fire(t, first)
	open := f.onlyCycle(t)
	if got := f.fire(t, first.Add(10*time.Minute)); got != "skipped_open_cycle" {
		t.Fatalf("second fire = %q, want skipped_open_cycle", got)
	}
	if c := f.cycle(t, open.ID); c.State != "draining" || c.FinishedAt.Valid {
		t.Fatalf("open cycle = %q finished %v, want it to continue", c.State, c.FinishedAt.Valid)
	}
	if p := f.pauseRow(t); p == nil || p.CycleID != open.ID {
		t.Fatalf("pause = %+v, want still held by the open cycle", p)
	}
	// The same cron time again is a no-op.
	if got := f.fire(t, first); got != "draining" {
		t.Fatalf("repeat of the first fire = %q, want its existing state draining", got)
	}
	if n := f.cycleCount(t); n != 2 {
		t.Fatalf("cycle rows = %d, want 2", n)
	}
}

// 9. Fire racing a manual resume, on two connections.
func TestFireUpdateCycle_RacesManualResume(t *testing.T) {
	t.Run("resume waits for the fire transaction", func(t *testing.T) {
		f := newFireFixture(t, "fire-race-resume", olderBuildCommit)
		f.setLiveRuns(t, 1)
		inserted, proceed := make(chan struct{}), make(chan struct{})
		fired := make(chan error, 1)
		go func() {
			_, err := testHandler.fireUpdateCycle(context.Background(), f.machine.OwnerID, f.machine.DaemonID, time.Now(), 20*time.Minute, func() error {
				close(inserted)
				<-proceed
				return nil
			})
			fired <- err
		}()
		select {
		case <-inserted:
		case err := <-fired:
			t.Fatalf("fire returned before its insert hook: %v", err)
		}
		code := make(chan int, 1)
		go func() { code <- deletePause(t, testUserID, f.runtimeID).Code }()
		select {
		case got := <-code:
			t.Fatalf("resume returned %d while the fire transaction held the machine lock", got)
		case <-time.After(400 * time.Millisecond):
		}
		close(proceed)
		if err := <-fired; err != nil {
			t.Fatalf("fire: %v", err)
		}
		if got := <-code; got != http.StatusNoContent {
			t.Fatalf("resume = %d", got)
		}
		c := f.onlyCycle(t)
		wantCancel(t, c, "manual_resume")
		if n := advance(t, time.Now()); n != 1 {
			t.Fatalf("advanced %d, want 1", n)
		}
		c = f.cycle(t, c.ID)
		if c.Outcome.String != "cancelled" {
			t.Fatalf("cycle = %q, want cancelled", c.Outcome.String)
		}
		if p := f.pauseRow(t); p != nil {
			t.Fatalf("pause = %+v, want none", p)
		}
	})
	t.Run("resume before fire", func(t *testing.T) {
		f := newFireFixture(t, "fire-race-first", olderBuildCommit)
		f.setLiveRuns(t, 1)
		if w := deletePause(t, testUserID, f.runtimeID); w.Code != http.StatusNoContent {
			t.Fatalf("resume = %d", w.Code)
		}
		f.fire(t, time.Now())
		c := f.onlyCycle(t)
		if c.State != "draining" || c.CancelRequested.Valid {
			t.Fatalf("cycle = %q cancel %v, want a draining cycle", c.State, c.CancelRequested)
		}
		if n := advance(t, time.Now()); n != 0 {
			t.Fatalf("advanced %d, want 0 (still draining)", n)
		}
	})
}

// 10. Idle and offline until the cutoff: no_window / offline; the next cron time opens a new cycle.
func TestAdvanceUpdateCycles_IdleOfflineEndsAtCutoff(t *testing.T) {
	f := newFireFixture(t, "fire-offline", olderBuildCommit)
	first := time.Now().Truncate(time.Millisecond)
	f.fire(t, first)
	c := f.onlyCycle(t)
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, f.runtimeID)
	// Before the cutoff an idle offline machine waits.
	if n := advance(t, c.DeadlineAt.Time.Add(-10*time.Minute)); n != 0 {
		t.Fatalf("advanced %d before the cutoff, want 0", n)
	}
	if n := advance(t, c.DeadlineAt.Time.Add(-3*time.Minute)); n != 1 {
		t.Fatalf("advanced %d at the cutoff, want 1", n)
	}
	c = f.cycle(t, c.ID)
	if c.Outcome.String != "no_window" || c.OutcomeDetail.String != "offline" || c.LiveRunsAtEnd.Int32 != 0 {
		t.Fatalf("cycle = %q / %q live %v, want no_window / offline / 0", c.Outcome.String, c.OutcomeDetail.String, c.LiveRunsAtEnd)
	}
	if p := f.pauseRow(t); p != nil {
		t.Fatalf("pause = %+v, want released", p)
	}
	if got := f.fire(t, first.Add(24*time.Hour)); got != "draining" {
		t.Fatalf("next cron time = %q, want a new draining cycle", got)
	}
}

// 4 of the driver steps: idle and online before the cutoff reaches ready_to_update, once.
func TestAdvanceUpdateCycles_ReadyToUpdateHook(t *testing.T) {
	f := newFireFixture(t, "fire-ready", olderBuildCommit)
	ready := readyCalls(t)
	f.setLiveRuns(t, 2)
	f.fire(t, time.Now())
	c := f.onlyCycle(t)
	if advance(t, time.Now()); ready.Load() != 0 {
		t.Fatalf("ready_to_update ran with 2 live runs")
	}
	f.setLiveRuns(t, 0)
	advance(t, time.Now())
	if got := ready.Load(); got != 1 {
		t.Fatalf("ready_to_update ran %d times with 0 runs and an online runtime, want 1", got)
	}
	if c = f.cycle(t, c.ID); c.FinishedAt.Valid {
		t.Fatalf("cycle finished by the hook step: %q", c.Outcome.String)
	}
}

// The runtime list shows the machine's last outcome and its time.
func TestFireUpdateCycle_LastOutcomeOnRuntimeList(t *testing.T) {
	f := newFireFixture(t, "fire-last-outcome", serverBuildCommit)
	f.fire(t, time.Now())
	rt := listRuntimesForTest(t)[f.runtimeID]
	if rt.LastUpdateOutcome == nil || *rt.LastUpdateOutcome != "up_to_date" || rt.LastUpdateOutcomeAt == nil {
		t.Fatalf("last outcome = %v at %v, want up_to_date with a time", rt.LastUpdateOutcome, rt.LastUpdateOutcomeAt)
	}
}
