package main

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2894 step 11 (CHE-2914): the compaction-only start bound. A compaction
// task that has not started within the `compact` claim deadline of the job
// catalog (CHE-4097), counted from the moment its DM became free, fails with
// compaction_start_timeout. Every case runs the sweeper's own round, so a
// dropped stage fails.

// pastBound is an age safely beyond the bound and well under every ordinary
// threshold (dispatchTimeoutSeconds is 300 s), so only the new stage can act.
var pastBound = bgagents.Catalog[bgagents.Compact].ClaimDeadline + 10*time.Second

func sweepRoundForCompaction(t *testing.T) {
	t.Helper()
	q := db.New(testPool)
	bus := events.New()
	runtimeSweepRound(ctx(), q, nil, service.NewTaskService(q, testPool, nil, bus), bus, defaultRuntimeReconnectGrace)
}

func ageTask(t *testing.T, id string, age time.Duration) {
	t.Helper()
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET created_at = now() - make_interval(secs => $2),
		dispatched_at = CASE WHEN dispatched_at IS NULL THEN NULL ELSE now() - make_interval(secs => $2) END WHERE id = $1`,
		id, age.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// compactionTask inserts a purpose='compaction' task in status on dm.
func compactionTask(t *testing.T, a admissionAgent, dm, status string, age time.Duration) string {
	t.Helper()
	id := addAdmissionTask(t, a, dm, "", status)
	markCompaction(t, id)
	if status == "dispatched" || status == "waiting_local_directory" {
		if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET dispatched_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	ageTask(t, id, age)
	return id
}

type taskRow struct{ status, reason, errText string }

func readTask(t *testing.T, id string) taskRow {
	t.Helper()
	var r taskRow
	if err := testPool.QueryRow(ctx(), `SELECT status, COALESCE(failure_reason, ''), COALESCE(error, '')
		FROM agent_task_queue WHERE id = $1`, id).Scan(&r.status, &r.reason, &r.errText); err != nil {
		t.Fatal(err)
	}
	return r
}

func wantExpired(t *testing.T, id, what string) {
	t.Helper()
	r := readTask(t, id)
	if r.status != "failed" || r.reason != "compaction_start_timeout" {
		t.Fatalf("%s: status=%s reason=%s, want failed/compaction_start_timeout", what, r.status, r.reason)
	}
	if r.errText == "" || strings.Contains(strings.ToLower(r.errText), "transcript:") {
		t.Fatalf("%s: error text %q must be set and carry no transcript content", what, r.errText)
	}
}

func wantStatus(t *testing.T, id, want, what string) {
	t.Helper()
	if got := readTask(t, id).status; got != want {
		t.Fatalf("%s: status=%s, want %s", what, got, want)
	}
}

// Healthy busy runtime: the runtime heartbeats and runs other work, so
// ExpireStaleQueuedTasks keeps the queue. A compaction past the bound still
// fails; an ordinary queued task of the same age on the same runtime stays.
func TestCompactionStartBoundFailsQueuedOnHealthyBusyRuntime(t *testing.T) {
	a := newAdmissionAgent(t, "cb-busy")
	dm := newAdmissionDM(t, a)
	busyDM := newAdmissionDM(t, a)
	addAdmissionTask(t, a, busyDM, "", "running")
	compaction := compactionTask(t, a, dm, "queued", pastBound)

	ordinaryDM := newAdmissionDM(t, a)
	ordinary := addAdmissionTask(t, a, ordinaryDM, "", "queued")
	ageTask(t, ordinary, pastBound)

	sweepRoundForCompaction(t)

	wantExpired(t, compaction, "queued compaction past the bound")
	wantStatus(t, ordinary, "queued", "ordinary queued task, same age, same runtime")
}

// A renewed prepare lease does not stop the clock for a compaction; it does for
// ordinary work (FailStaleTasks keeps its lease rule).
func TestCompactionStartBoundIgnoresRenewedPrepareLease(t *testing.T) {
	a := newAdmissionAgent(t, "cb-lease")
	dm := newAdmissionDM(t, a)
	compaction := compactionTask(t, a, dm, "dispatched", pastBound)
	ordinaryDM := newAdmissionDM(t, a)
	ordinary := addAdmissionTask(t, a, ordinaryDM, "", "dispatched")
	for _, id := range []string{compaction, ordinary} {
		if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET dispatched_at = now() - make_interval(secs => $2),
			prepare_lease_expires_at = now() + interval '10 minutes' WHERE id = $1`, id, pastBound.Seconds()); err != nil {
			t.Fatal(err)
		}
	}

	sweepRoundForCompaction(t)

	wantExpired(t, compaction, "dispatched compaction with a live lease")
	wantStatus(t, ordinary, "dispatched", "ordinary dispatched task with a live lease")
}

func TestCompactionStartBoundFailsWaitingLocalDirectory(t *testing.T) {
	a := newAdmissionAgent(t, "cb-localdir")
	dm := newAdmissionDM(t, a)
	compaction := compactionTask(t, a, dm, "waiting_local_directory", pastBound)

	sweepRoundForCompaction(t)

	wantExpired(t, compaction, "compaction waiting for a local directory")
}

// A compaction queued behind a running DM turn waits for serialization, not
// for its runtime: its clock starts when the DM becomes free.
func TestCompactionStartBoundCountsFromWhenTheDMBecameFree(t *testing.T) {
	r1, r2 := newAdmissionAgent(t, "cb-ser1"), newAdmissionAgent(t, "cb-ser2")
	dm := newAdmissionDM(t, r1)
	turn := addAdmissionTask(t, r1, dm, "", "running")
	compaction := compactionTask(t, r2, dm, "queued", 10*time.Minute)

	sweepRoundForCompaction(t)
	wantStatus(t, compaction, "queued", "compaction behind a running DM turn")

	// The turn ends just now: the clock starts at its completed_at.
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, turn); err != nil {
		t.Fatal(err)
	}
	sweepRoundForCompaction(t)
	wantStatus(t, compaction, "queued", "compaction right after the DM became free")

	// Past the bound measured from that completed_at.
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET completed_at = now() - make_interval(secs => $2) WHERE id = $1`,
		turn, pastBound.Seconds()); err != nil {
		t.Fatal(err)
	}
	sweepRoundForCompaction(t)
	wantExpired(t, compaction, "compaction past the bound since the DM became free")
}

// Claim/start vs expiry, both orders, on two connections.
func TestCompactionStartBoundRacesWithStart(t *testing.T) {
	a := newAdmissionAgent(t, "cb-race")
	dm := newAdmissionDM(t, a)

	// Start first: a connection holds the row and starts the task; the sweep
	// skips the locked row, and once committed the task is no longer a victim.
	started := compactionTask(t, a, dm, "dispatched", pastBound)
	tx, err := testPool.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx())
	if _, err := tx.Exec(ctx(), `SELECT 1 FROM agent_task_queue WHERE id = $1 FOR UPDATE`, started); err != nil {
		t.Fatal(err)
	}
	sweepRoundForCompaction(t)
	wantStatus(t, started, "dispatched", "locked row during the sweep")
	if _, err := tx.Exec(ctx(), `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, started); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx()); err != nil {
		t.Fatal(err)
	}
	sweepRoundForCompaction(t)
	wantStatus(t, started, "running", "started task is no longer a victim")

	// Expiry first (own DM, so the first part's completed_at does not move the
	// age origin): the sweep fails the task, then a claim finds nothing and a
	// later start is refused.
	expired := compactionTask(t, a, newAdmissionDM(t, a), "queued", pastBound)
	sweepRoundForCompaction(t)
	wantExpired(t, expired, "expiry before claim")
	got, err := claimRuntime(t, admissionService(), a)
	if err != nil || got != "" {
		t.Fatalf("claim after expiry = %q, %v; want no task, no error", got, err)
	}
	if _, err := admissionService().StartTask(ctx(), ruleEUUID(t, expired)); err == nil {
		t.Fatal("start after expiry succeeded, want refusal")
	}
	wantExpired(t, expired, "start after expiry")
}

// A running compaction follows today's running policy: the bound never fails it.
func TestCompactionStartBoundLeavesRunningCompactionAlone(t *testing.T) {
	a := newAdmissionAgent(t, "cb-running")
	dm := newAdmissionDM(t, a)
	running := compactionTask(t, a, dm, "running", time.Hour)
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET started_at = now() - interval '30 minutes',
		dispatched_at = now() - interval '31 minutes' WHERE id = $1`, running); err != nil {
		t.Fatal(err)
	}

	sweepRoundForCompaction(t)

	wantStatus(t, running, "running", "running compaction on a healthy runtime")
}
