package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Scheduled update cycle owns the machine pause (CHE-5466, D6, D10, D18).

// cycleFixture is one machine with one runtime, cleaned of cycle and pause rows.
type cycleFixture struct {
	runtimeID string
	machine   CycleMachine
}

func newCycleFixture(t *testing.T, daemonID string) cycleFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID, _ := seedPausableRuntime(t, context.Background(), daemonID, "Cycle "+daemonID)
	rt, err := testHandler.Queries.GetAgentRuntime(context.Background(), parseUUID(runtimeID))
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	clean := func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_update_cycle WHERE daemon_id = $1`, daemonID)
		testPool.Exec(context.Background(), `DELETE FROM daemon_pause WHERE daemon_id = $1`, daemonID)
	}
	clean()
	t.Cleanup(clean)
	return cycleFixture{
		runtimeID: runtimeID,
		machine:   CycleMachine{OwnerID: machineOwnerID(rt), DaemonID: daemonID, WorkspaceID: rt.WorkspaceID},
	}
}

func (f cycleFixture) open(t *testing.T, window time.Duration) db.DaemonUpdateCycle {
	t.Helper()
	c, err := testHandler.OpenCycle(context.Background(), f.machine, time.Now().Truncate(time.Millisecond), window)
	if err != nil {
		t.Fatalf("OpenCycle: %v", err)
	}
	return c
}

func (f cycleFixture) pauseRow(t *testing.T) *db.DaemonPause {
	t.Helper()
	var p db.DaemonPause
	err := testPool.QueryRow(context.Background(),
		`SELECT machine_owner_id, daemon_id, workspace_id, reason, paused_by, paused_at, drained_notified_at, cycle_id, expires_at
		 FROM daemon_pause WHERE daemon_id = $1`, f.machine.DaemonID).
		Scan(&p.MachineOwnerID, &p.DaemonID, &p.WorkspaceID, &p.Reason, &p.PausedBy, &p.PausedAt, &p.DrainedNotifiedAt, &p.CycleID, &p.ExpiresAt)
	if err != nil {
		return nil
	}
	return &p
}

func (f cycleFixture) cycle(t *testing.T, id pgtype.UUID) db.DaemonUpdateCycle {
	t.Helper()
	c, err := testHandler.Queries.GetDaemonUpdateCycle(context.Background(), id)
	if err != nil {
		t.Fatalf("read cycle: %v", err)
	}
	return c
}

func (f cycleFixture) cycleCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM daemon_update_cycle WHERE daemon_id = $1`, f.machine.DaemonID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f cycleFixture) claimed(t *testing.T) string { return singleClaimTaskID(t, f.runtimeID) }

func wantCancel(t *testing.T, c db.DaemonUpdateCycle, want string) {
	t.Helper()
	if !c.CancelRequested.Valid || c.CancelRequested.String != want {
		t.Fatalf("cancel_requested = %+v, want %q", c.CancelRequested, want)
	}
}

// 1. Acquire.
func TestCyclePause_Acquire(t *testing.T) {
	t.Run("no pause", func(t *testing.T) {
		f := newCycleFixture(t, "cycle-acquire-none")
		c := f.open(t, 20*time.Minute)
		if c.State != "draining" {
			t.Fatalf("state = %q, want draining", c.State)
		}
		p := f.pauseRow(t)
		if p == nil || p.Reason != "scheduled_update" || p.CycleID != c.ID || !p.ExpiresAt.Valid || p.PausedBy.Valid {
			t.Fatalf("pause = %+v, want scheduled_update owned by cycle %v with expires_at and no paused_by", p, c.ID)
		}
		if !p.ExpiresAt.Time.Equal(c.DeadlineAt.Time) {
			t.Fatalf("pause expires_at %v != cycle deadline_at %v", p.ExpiresAt.Time, c.DeadlineAt.Time)
		}
		if got := f.claimed(t); got != "" {
			t.Fatalf("claimed %q while the cycle holds the pause", got)
		}
	})
	t.Run("live manual pause", func(t *testing.T) {
		f := newCycleFixture(t, "cycle-acquire-live")
		pauseDaemonForTest(t, f.machine.DaemonID)
		before := f.pauseRow(t)
		c := f.open(t, 20*time.Minute)
		if c.State != "skipped_manual_pause" || !c.FinishedAt.Valid || c.Outcome.String != "skipped_manual_pause" {
			t.Fatalf("cycle = state %q outcome %+v finished %v, want a finished skipped_manual_pause", c.State, c.Outcome, c.FinishedAt.Valid)
		}
		after := f.pauseRow(t)
		if after == nil || after.Reason != "maintenance" || after.CycleID.Valid || !after.PausedAt.Time.Equal(before.PausedAt.Time) {
			t.Fatalf("manual pause changed: before %+v after %+v", before, after)
		}
	})
	t.Run("expired manual pause", func(t *testing.T) {
		f := newCycleFixture(t, "cycle-acquire-expired")
		dbfx.Exec(t, `INSERT INTO daemon_pause (machine_owner_id, daemon_id, workspace_id, reason, paused_by, paused_at)
			VALUES ($1, $2, $3, 'incident', $4, now() - interval '31 minutes')`, f.machine.OwnerID, f.machine.DaemonID, f.machine.WorkspaceID, testUserID)
		c := f.open(t, 20*time.Minute)
		if c.State != "draining" {
			t.Fatalf("state = %q, want draining", c.State)
		}
		if p := f.pauseRow(t); p == nil || p.Reason != "scheduled_update" || p.CycleID != c.ID {
			t.Fatalf("pause = %+v, want taken over by cycle", p)
		}
	})
}

// 2. A person pauses after the acquire.
func TestCyclePause_ManualPauseAfterAcquireWins(t *testing.T) {
	f := newCycleFixture(t, "cycle-pause-after")
	c := f.open(t, 20*time.Minute)
	if w := putPause(t, testUserID, f.runtimeID, map[string]any{"reason": "incident"}); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body.String())
	}
	p := f.pauseRow(t)
	if p == nil || p.Reason != "incident" || p.CycleID.Valid || p.ExpiresAt.Valid {
		t.Fatalf("pause = %+v, want the person's incident pause with no cycle_id and no expires_at", p)
	}
	wantCancel(t, f.cycle(t, c.ID), "manual_pause")
	n, err := testHandler.Queries.ReleaseCyclePause(context.Background(), db.ReleaseCyclePauseParams{MachineOwnerID: f.machine.OwnerID, DaemonID: f.machine.DaemonID, CycleID: c.ID})
	if err != nil || n != 0 {
		t.Fatalf("release = %d rows, err %v, want 0", n, err)
	}
	if p := f.pauseRow(t); p == nil || p.Reason != "incident" {
		t.Fatalf("person's pause gone after release: %+v", p)
	}
	if got := f.claimed(t); got != "" {
		t.Fatalf("claimed %q under the person's pause", got)
	}
}

// 3. A person pauses before the acquire.
func TestCyclePause_ManualPauseBeforeAcquireWins(t *testing.T) {
	f := newCycleFixture(t, "cycle-pause-before")
	if w := putPause(t, testUserID, f.runtimeID, map[string]any{"reason": "maintenance"}); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body.String())
	}
	c := f.open(t, 20*time.Minute)
	if c.State != "skipped_manual_pause" {
		t.Fatalf("state = %q, want skipped_manual_pause", c.State)
	}
	if p := f.pauseRow(t); p == nil || p.Reason != "maintenance" || p.CycleID.Valid {
		t.Fatalf("pause = %+v, want the person's pause", p)
	}
}

// 4. A person resumes after the acquire.
func TestCyclePause_ManualResumeAfterAcquireCancels(t *testing.T) {
	f := newCycleFixture(t, "cycle-resume-after")
	c := f.open(t, 20*time.Minute)
	if w := deletePause(t, testUserID, f.runtimeID); w.Code != http.StatusNoContent {
		t.Fatalf("resume = %d: %s", w.Code, w.Body.String())
	}
	if p := f.pauseRow(t); p != nil {
		t.Fatalf("pause after resume = %+v, want none", p)
	}
	wantCancel(t, f.cycle(t, c.ID), "manual_resume")

	// Resume, pause again, then the cycle releases: the new pause stays.
	if w := putPause(t, testUserID, f.runtimeID, map[string]any{"reason": "suspended"}); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body.String())
	}
	n, err := testHandler.Queries.ReleaseCyclePause(context.Background(), db.ReleaseCyclePauseParams{MachineOwnerID: f.machine.OwnerID, DaemonID: f.machine.DaemonID, CycleID: c.ID})
	if err != nil || n != 0 {
		t.Fatalf("release = %d rows, err %v, want 0", n, err)
	}
	if p := f.pauseRow(t); p == nil || p.Reason != "suspended" {
		t.Fatalf("new pause = %+v, want the person's suspended pause", p)
	}
}

func TestCyclePause_ReleaseDeletesOwnPause(t *testing.T) {
	f := newCycleFixture(t, "cycle-release-own")
	c := f.open(t, 20*time.Minute)
	n, err := testHandler.Queries.ReleaseCyclePause(context.Background(), db.ReleaseCyclePauseParams{MachineOwnerID: f.machine.OwnerID, DaemonID: f.machine.DaemonID, CycleID: c.ID})
	if err != nil || n != 1 {
		t.Fatalf("release = %d rows, err %v, want 1", n, err)
	}
	if p := f.pauseRow(t); p != nil {
		t.Fatalf("pause after release = %+v, want none", p)
	}
}

// 5. A scheduled_update pause is not live after expires_at.
func TestCyclePause_NotLiveAfterExpiresAt(t *testing.T) {
	f := newCycleFixture(t, "cycle-expiry")
	taskID := f.claimedTaskIDFor(t)
	c := f.open(t, 20*time.Minute)
	if got := f.claimed(t); got != "" {
		t.Fatalf("claimed %q while live", got)
	}
	if hb := testHandler.heartbeatPause(context.Background(), f.runtimeID, nil); hb == nil || hb.Reason != "scheduled_update" {
		t.Fatalf("heartbeat pause = %+v, want scheduled_update", hb)
	}
	if p := listRuntimesForTest(t)[f.runtimeID].Pause; p == nil || p.Reason != "scheduled_update" {
		t.Fatalf("listed pause = %+v, want scheduled_update", p)
	}
	// paused_at is fresh, so only expires_at can end it.
	dbfx.Exec(t, `UPDATE daemon_pause SET expires_at = now() - interval '1 second' WHERE cycle_id = $1`, c.ID)
	if hb := testHandler.heartbeatPause(context.Background(), f.runtimeID, nil); hb != nil {
		t.Fatalf("heartbeat pause after expiry = %+v, want nil", hb)
	}
	if p := listRuntimesForTest(t)[f.runtimeID].Pause; p != nil {
		t.Fatalf("listed pause after expiry = %+v, want nil", p)
	}
	if got := f.claimed(t); got != taskID {
		t.Fatalf("claimed %q after expiry, want %s", got, taskID)
	}
}

// claimedTaskIDFor returns the queued task seeded for the fixture's runtime.
func (f cycleFixture) claimedTaskIDFor(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text FROM agent_task_queue WHERE runtime_id = $1 AND status = 'queued'`, f.runtimeID).Scan(&id); err != nil {
		t.Fatalf("queued task: %v", err)
	}
	return id
}

// 6. The deadline CHECK.
func TestCycle_DeadlineCheckRejectsOver30Minutes(t *testing.T) {
	f := newCycleFixture(t, "cycle-check")
	ins := func(window string) error {
		_, err := testPool.Exec(context.Background(), `INSERT INTO daemon_update_cycle (id, machine_owner_id, daemon_id, fire_at, started_at, deadline_at, state)
			VALUES ($1, $2, $3, now(), now(), now() + $4::interval, 'draining')`, dbid.NewV7(), f.machine.OwnerID, f.machine.DaemonID, window)
		return err
	}
	if err := ins("31 minutes"); err == nil {
		t.Fatal("31-min cycle accepted, want the deadline CHECK to reject it")
	}
	if err := ins("30 minutes"); err != nil {
		t.Fatalf("30-min cycle rejected: %v", err)
	}
}

// 7. Terra revision 4 P1 #1: two connections, OpenCycle holds the machine lock
// after its cycle insert.
func TestCycle_OpenCycleRacesManualActions(t *testing.T) {
	type op struct {
		name string
		run  func(t *testing.T, f cycleFixture) int
	}
	resume := op{"resume", func(t *testing.T, f cycleFixture) int { return deletePause(t, testUserID, f.runtimeID).Code }}
	pause := op{"pause", func(t *testing.T, f cycleFixture) int {
		return putPause(t, testUserID, f.runtimeID, map[string]any{"reason": "incident"}).Code
	}}

	for _, o := range []op{resume, pause} {
		t.Run(o.name+" waits for OpenCycle", func(t *testing.T) {
			f := newCycleFixture(t, "cycle-race-"+o.name+"-wait")
			inserted := make(chan struct{})
			proceed := make(chan struct{})
			type opened struct {
				c   db.DaemonUpdateCycle
				err error
			}
			openDone := make(chan opened, 1)
			go func() {
				c, err := testHandler.openCycle(context.Background(), f.machine, time.Now(), 20*time.Minute, func() error {
					close(inserted)
					<-proceed
					return nil
				})
				openDone <- opened{c, err}
			}()
			select {
			case <-inserted:
			case op := <-openDone:
				t.Fatalf("OpenCycle returned before its cycle insert hook: %+v, %v", op.c.State, op.err)
			}
			code := make(chan int, 1)
			go func() { code <- o.run(t, f) }()
			select {
			case got := <-code:
				t.Fatalf("%s returned %d while OpenCycle held the machine lock", o.name, got)
			case <-time.After(400 * time.Millisecond):
			}
			close(proceed)
			op := <-openDone
			if op.err != nil {
				t.Fatalf("OpenCycle: %v", op.err)
			}
			if got := <-code; got != http.StatusNoContent && got != http.StatusOK {
				t.Fatalf("%s = %d", o.name, got)
			}
			c := f.cycle(t, op.c.ID)
			wantCancel(t, c, "manual_"+o.name)
			p := f.pauseRow(t)
			if o.name == "resume" && p != nil {
				t.Fatalf("pause after resume = %+v, want none", p)
			}
			if o.name == "pause" && (p == nil || p.Reason != "incident" || p.CycleID.Valid) {
				t.Fatalf("pause = %+v, want the person's live incident pause", p)
			}
		})
		t.Run(o.name+" before OpenCycle", func(t *testing.T) {
			f := newCycleFixture(t, "cycle-race-"+o.name+"-first")
			if got := o.run(t, f); got != http.StatusNoContent && got != http.StatusOK {
				t.Fatalf("%s = %d", o.name, got)
			}
			c := f.open(t, 20*time.Minute)
			if c.CancelRequested.Valid {
				t.Fatalf("cancel_requested = %+v, want none: the %s came before the cycle", c.CancelRequested, o.name)
			}
			p := f.pauseRow(t)
			if o.name == "resume" && (c.State != "draining" || p == nil || p.CycleID != c.ID) {
				t.Fatalf("cycle %q pause %+v, want a draining cycle holding its pause", c.State, p)
			}
			if o.name == "pause" && (c.State != "skipped_manual_pause" || p == nil || p.Reason != "incident" || p.CycleID.Valid) {
				t.Fatalf("cycle %q pause %+v, want skipped_manual_pause and the person's pause", c.State, p)
			}
		})
	}
}

// 8. A failure after the cycle insert rolls the whole open back.
func TestCycle_OpenCycleRollsBackOnFailure(t *testing.T) {
	f := newCycleFixture(t, "cycle-rollback")
	boom := errors.New("injected")
	_, err := testHandler.openCycle(context.Background(), f.machine, time.Now(), 20*time.Minute, func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if n := f.cycleCount(t); n != 0 {
		t.Fatalf("%d cycle rows after rollback, want 0", n)
	}
	if p := f.pauseRow(t); p != nil {
		t.Fatalf("pause after rollback = %+v, want none", p)
	}
}

// 9. The heartbeat carries expires_in_ms.
func TestCyclePause_HeartbeatCarriesExpiresInMs(t *testing.T) {
	f := newCycleFixture(t, "cycle-hb-ms")
	f.open(t, 20*time.Minute)
	hb := testHandler.heartbeatPause(context.Background(), f.runtimeID, nil)
	if hb == nil || hb.ExpiresInMs == nil {
		t.Fatalf("heartbeat pause = %+v, want expires_in_ms", hb)
	}
	expires, err := time.Parse(time.RFC3339Nano, hb.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Until(expires).Milliseconds()
	if d := *hb.ExpiresInMs - want; d < -1000 || d > 1000 {
		t.Fatalf("expires_in_ms = %d, want %d (expires_at minus now) within 1 s", *hb.ExpiresInMs, want)
	}
	if *hb.ExpiresInMs > 20*60*1000 || *hb.ExpiresInMs < 19*60*1000 {
		t.Fatalf("expires_in_ms = %d, want about the 20-min window", *hb.ExpiresInMs)
	}
}

func TestPause_HeartbeatCarriesExpiresInMsForManualPause(t *testing.T) {
	f := newCycleFixture(t, "cycle-hb-ms-manual")
	if w := putPause(t, testUserID, f.runtimeID, map[string]any{"reason": "incident"}); w.Code != http.StatusOK {
		t.Fatalf("pause = %d", w.Code)
	}
	hb := testHandler.heartbeatPause(context.Background(), f.runtimeID, nil)
	if hb == nil || hb.ExpiresInMs == nil || *hb.ExpiresInMs < 29*60*1000 || *hb.ExpiresInMs > 30*60*1000 {
		t.Fatalf("heartbeat pause = %+v, want expires_in_ms about 30 min", hb)
	}
}

// 10. A cron time that fires while the machine's previous cycle is open.
func TestCycle_OpenCycleWhileOpenRecordsSkipped(t *testing.T) {
	f := newCycleFixture(t, "cycle-open-twice")
	first := f.open(t, 20*time.Minute)
	pauseBefore := f.pauseRow(t)

	second, err := testHandler.OpenCycle(context.Background(), f.machine, time.Now().Add(time.Minute).Truncate(time.Millisecond), 20*time.Minute)
	if err != nil {
		t.Fatalf("OpenCycle while open: %v (want a recorded skip, not an index error)", err)
	}
	if second.Outcome.String != "skipped_open_cycle" || second.OutcomeSource.String != "server" || !second.FinishedAt.Valid {
		t.Fatalf("second = %+v, want a finished skipped_open_cycle with source server", second)
	}
	if got := f.cycle(t, first.ID); got.FinishedAt.Valid || got.State != "draining" {
		t.Fatalf("open cycle changed: %+v", got)
	}
	after := f.pauseRow(t)
	if after == nil || after.CycleID != first.ID || !after.PausedAt.Time.Equal(pauseBefore.PausedAt.Time) || !after.ExpiresAt.Time.Equal(pauseBefore.ExpiresAt.Time) {
		t.Fatalf("pause changed: before %+v after %+v", pauseBefore, after)
	}
}
