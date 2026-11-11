package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5265 (CHE-5213 slice 1): a release owed by an ended run survives a failed,
// skipped or never-run fast path and is delivered by the job.

func (e *admEnv) obligations() []string {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT task_id::text FROM admission_release WHERE project_id = $1 ORDER BY created_at, task_id`, e.project)
	if err != nil {
		e.t.Fatalf("obligations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			e.t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	e.t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM admission_release WHERE project_id = $1`, e.project)
	})
	return out
}

func (e *admEnv) addObligation(task pgtype.UUID) {
	e.t.Helper()
	e.t.Cleanup(func() {
		e.pool.Exec(context.Background(), `DELETE FROM admission_release WHERE project_id = $1`, e.project)
	})
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO admission_release (task_id, project_id) VALUES ($1, $2)`, task, e.project); err != nil {
		e.t.Fatalf("insert obligation: %v", err)
	}
}

// endWithoutFastPath commits a run's terminal state through the one settle
// entry point and never calls releaseAfterRun: the crash-before-fast-path case.
func (e *admEnv) endWithoutFastPath(task *db.AgentTaskQueue) {
	e.t.Helper()
	e.start(task)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := e.svc.Queries.WithTx(tx)
	ended, err := q.CompleteAgentTask(ctx, db.CompleteAgentTaskParams{ID: task.ID, Result: []byte(`{}`)})
	if err != nil {
		e.t.Fatalf("complete row: %v", err)
	}
	if err := SettleTerminalTaskState(ctx, q, ended); err != nil {
		e.t.Fatalf("settle: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// lockHeld holds a row lock on a held run in another transaction, so the
// release's FOR UPDATE SKIP LOCKED skips it. The returned func releases it.
func (e *admEnv) lockHeld(id pgtype.UUID) func() {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_task_queue WHERE id = $1 FOR UPDATE`, id); err != nil {
		e.t.Fatalf("lock held run: %v", err)
	}
	return func() { tx.Rollback(ctx) }
}

// 1: the row commits with the terminal state; a rolled-back end leaves none.
func TestAdmissionReleaseRowCommitsWithTerminalStateOnly(t *testing.T) {
	e := newAdmEnv(t)
	e.enqueue(e.issue("medium"))
	run := e.claim()
	e.start(run)
	ctx := context.Background()

	tx, _ := e.pool.Begin(ctx)
	q := e.svc.Queries.WithTx(tx)
	ended, err := q.CompleteAgentTask(ctx, db.CompleteAgentTaskParams{ID: run.ID, Result: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := SettleTerminalTaskState(ctx, q, ended); err != nil {
		t.Fatal(err)
	}
	if got := e.obligations(); len(got) != 0 {
		t.Fatalf("uncommitted end shows rows %v to other sessions", got)
	}
	tx.Rollback(ctx)
	if got := e.obligations(); len(got) != 0 {
		t.Fatalf("rolled-back end left rows %v", got)
	}

	e.endWithoutFastPath(run)
	if got := e.obligations(); len(got) != 1 || got[0] != util.UUIDToString(run.ID) {
		t.Fatalf("rows after the committed end = %v, want [%s]", got, util.UUIDToString(run.ID))
	}
}

// 2: a failed project lookup aborts the end.
func TestAdmissionReleaseLookupFailureAbortsTheEnd(t *testing.T) {
	e := newAdmEnv(t)
	ghost := db.AgentTaskQueue{ID: util.MustParseUUID("00000000-0000-7000-8000-000000000f05"), Status: "completed"}
	tx, _ := e.pool.Begin(context.Background())
	defer tx.Rollback(context.Background())
	if err := SettleTerminalTaskState(context.Background(), e.svc.Queries.WithTx(tx), ghost); err == nil {
		t.Fatal("settle succeeded although the task's project lookup failed")
	}
}

// 3: a held run locked by another transaction keeps the row; a later job tick
// admits it with no restart.
func TestAdmissionReleaseSkippedHeldRunIsDeliveredByJob(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	e.enqueue(e.issue("medium"))
	run := e.claim()
	held := e.enqueue(e.issue("medium"))
	if e.claim() != nil || e.where(held.ID) != "queued/held" {
		t.Fatalf("second run = %s, want queued/held", e.where(held.ID))
	}
	unlock := e.lockHeld(held.ID)
	e.complete(run) // fast path runs release while the held row is locked
	if got := e.obligations(); len(got) != 1 {
		unlock()
		t.Fatalf("obligation rows after a skipped release = %v, want the ended run's row", got)
	}
	if w := e.where(held.ID); w != "queued/held" {
		unlock()
		t.Fatalf("held run = %s while locked, want queued/held", w)
	}
	unlock()
	if _, err := e.svc.ReleaseObligations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := e.where(held.ID); w != "queued/reserved" {
		t.Fatalf("held run after the job = %s, want queued/reserved", w)
	}
	if got := e.obligations(); len(got) != 0 {
		t.Fatalf("rows after delivery = %v, want none", got)
	}
}

// 4: a closed project (paused, or full) has nothing the release skipped; the row goes.
func TestAdmissionReleaseClosedProjectDeletesRow(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	e.enqueue(e.issue("medium"))
	run := e.claim() // holds the one slot
	held := e.enqueue(e.issue("medium"))
	e.claim()
	other := e.enqueue(e.issue("medium"))
	e.addObligation(other.ID)
	if err := e.svc.ReleaseProjectAdmission(context.Background(), util.MustParseUUID(e.project)); err != nil {
		t.Fatal(err)
	}
	if got := e.obligations(); len(got) != 0 {
		t.Fatalf("rows after a release of a full project = %v, want none", got)
	}
	e.addObligation(other.ID)
	e.setStatus("paused")
	if err := e.svc.ReleaseProjectAdmission(context.Background(), util.MustParseUUID(e.project)); err != nil {
		t.Fatal(err)
	}
	if got := e.obligations(); len(got) != 0 {
		t.Fatalf("rows after a release of a paused project = %v, want none", got)
	}
	if w := e.where(held.ID); w != "queued/held" {
		t.Fatalf("held run = %s, want queued/held while the project is closed", w)
	}
	_ = run
}

type failingTx struct{ TxStarter }

func (failingTx) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("begin refused") }

// 5: a failed release leaves the row; the job delivers.
func TestAdmissionReleaseFailureLeavesRowAndJobDelivers(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	e.enqueue(e.issue("medium"))
	run := e.claim()
	held := e.enqueue(e.issue("medium"))
	e.claim()
	// The ending transaction is the real one; the fast path's release cannot begin.
	e.endWithoutFastPath(run)
	broken := &TaskService{Queries: e.svc.Queries, TxStarter: failingTx{e.pool}, Bus: e.svc.Bus}
	broken.releaseAfterRun(*run)
	if got := e.obligations(); len(got) != 1 {
		t.Fatalf("rows after a failed fast path = %v, want 1", got)
	}
	if _, err := e.svc.ReleaseObligations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := e.where(held.ID); w != "queued/reserved" {
		t.Fatalf("held run after the job = %s, want queued/reserved", w)
	}
}

// 6: two deliveries of one release: one admission, rows deleted once.
func TestAdmissionReleaseRepeatedDeliveryAdmitsOnce(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	e.enqueue(e.issue("medium"))
	run := e.claim()
	h1 := e.enqueue(e.issue("medium"))
	h2 := e.enqueue(e.issue("medium"))
	e.claim()
	e.claim()
	e.endWithoutFastPath(run)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.ReleaseObligations(context.Background())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if e.wip() != 1 {
		t.Fatalf("WIP after two deliveries = %d, want 1 (one admission)", e.wip())
	}
	if w1, w2 := e.where(h1.ID), e.where(h2.ID); w1 != "queued/reserved" || w2 != "queued/held" {
		t.Fatalf("held runs = %s, %s; want queued/reserved, queued/held", w1, w2)
	}
	if got := e.obligations(); len(got) != 0 {
		t.Fatalf("rows after delivery = %v, want none", got)
	}
}

// 7: the process died before the fast path ran; the job admits the held run.
func TestAdmissionReleaseJobAdmitsAfterCrashBeforeFastPath(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	e.enqueue(e.issue("medium"))
	run := e.claim()
	held := e.enqueue(e.issue("medium"))
	e.claim()
	e.endWithoutFastPath(run)
	if w := e.where(held.ID); w != "queued/held" {
		t.Fatalf("held run = %s before any release, want queued/held", w)
	}
	if n, err := e.svc.ReleaseObligations(context.Background()); err != nil || n != 1 {
		t.Fatalf("ReleaseObligations = %d, %v; want 1 project, nil", n, err)
	}
	if w := e.where(held.ID); w != "queued/reserved" {
		t.Fatalf("held run after the job = %s, want queued/reserved", w)
	}
}

// 8: a row inserted after the release read its rows stays.
func TestAdmissionReleaseKeepsRowsInsertedAfterItStarted(t *testing.T) {
	e := newAdmEnv(t)
	first := e.enqueue(e.issue("medium"))
	second := e.enqueue(e.issue("medium"))
	e.addObligation(first.ID)
	e.svc.afterReleaseRead = func() { e.addObligation(second.ID) }
	if err := e.svc.ReleaseProjectAdmission(context.Background(), util.MustParseUUID(e.project)); err != nil {
		t.Fatal(err)
	}
	got := e.obligations()
	if len(got) != 1 || got[0] != util.UUIDToString(second.ID) {
		t.Fatalf("rows after the release = %v, want only the later row %s", got, util.UUIDToString(second.ID))
	}
}

// 9: a run without a project owes no release.
func TestAdmissionReleaseRunWithoutProjectInsertsNoRow(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	if _, err := e.pool.Exec(context.Background(), `UPDATE issue SET project_id = NULL WHERE id = $1`, issue); err != nil {
		t.Fatal(err)
	}
	e.enqueue(issue)
	run := e.claim()
	if run == nil {
		t.Fatal("project-less run did not start")
	}
	e.endWithoutFastPath(run)
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM admission_release WHERE task_id = $1`, run.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows for a project-less run = %d (err %v), want 0", n, err)
	}
}
