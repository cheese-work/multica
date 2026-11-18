package handler

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Child-follow transaction, lock order and event watermark (CHE-3473 T3d, M11).

const followRaceWait = 3 * time.Second

// blocked reports whether done stays open for a short while: the call behind it
// is waiting on a lock.
func followBlocked(done <-chan error) bool {
	select {
	case <-done:
		return false
	case <-time.After(400 * time.Millisecond):
		return true
	}
}

func followFinished(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(followRaceWait):
		t.Fatalf("%s did not finish within %v", what, followRaceWait)
	}
}

func followBeginTx(t *testing.T) (pgx.Tx, *db.Queries) {
	t.Helper()
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	return tx, testHandler.Queries.WithTx(tx)
}

func followIssueOf(t *testing.T, id string) db.Issue {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(t.Context(), uuidOf(id))
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

// addManual follows issue in lane on its own transaction.
func followAddManual(ctx context.Context, lane string, issue db.Issue) (db.DmProgressFollow, error) {
	tx, err := testPool.Begin(ctx)
	if err != nil {
		return db.DmProgressFollow{}, err
	}
	defer tx.Rollback(context.Background())
	f, _, err := service.AddProgressFollow(ctx, testHandler.Queries.WithTx(tx), service.DMLane{Lineage: uuidOf(lane), Owner: uuidOf(testUserID)}, issue, []string{service.ReasonManual}, issue.ID, pgtype.Timestamptz{})
	if err != nil {
		return f, err
	}
	return f, tx.Commit(ctx)
}

// AC3 / M11: the follow path never waits for the lane row. Only the dispatcher
// locks it, and it skips a held lane instead of waiting.
func TestDMProgressFollow_WritesDoNotWaitForTheLaneLock(t *testing.T) {
	e := newCapWorld(t)
	other := followIssueOf(t, dbfx.Issue(t, "follow lock "+t.Name()))
	parent := followIssueOf(t, e.issue)
	child := followIssueOf(t, dbfx.Issue(t, "follow lock child "+t.Name()))

	holder, hq := followBeginTx(t)
	if _, err := hq.LockDMProgressLaneSkipLocked(t.Context(), uuidOf(e.lane)); err != nil {
		t.Fatalf("hold the lane like a dispatcher: %v", err)
	}
	defer holder.Rollback(context.Background())

	writes := map[string]func() error{
		"follow": func() error { _, err := followAddManual(t.Context(), e.lane, other); return err },
		"mute": func() error {
			rec := httptest.NewRecorder()
			testHandler.MuteIssueProgress(rec, progressReq("POST", e.issue, "", e.lane))
			if rec.Code != 200 {
				t.Errorf("mute = %d: %s", rec.Code, rec.Body.String())
			}
			return nil
		},
		"inherit": func() error {
			tx, err := testPool.Begin(t.Context())
			if err != nil {
				return err
			}
			defer tx.Rollback(context.Background())
			if err := service.InheritProgressFollows(t.Context(), testHandler.Queries.WithTx(tx), parent, child); err != nil {
				return err
			}
			return tx.Commit(t.Context())
		},
	}
	for name, write := range writes {
		done := make(chan error, 1)
		go func() { done <- write() }()
		followFinished(t, name+" while a dispatcher holds the lane", done)
	}
}

// AC1 / M11, enumeration side: a follow that arrives while a child create has
// enumerated the parent's follows but not yet committed waits for that create,
// so the child never commits unfollowed after the follow committed.
func TestDMProgressFollow_FollowWaitsForAnOpenChildCreate(t *testing.T) {
	w := newDMWorld(t)
	lane := w.dm(t, testUserID)
	parent := followIssueOf(t, dbfx.Issue(t, "race parent "+t.Name()))
	child := followIssueOf(t, dbfx.Issue(t, "race child "+t.Name()))

	tx, q := followBeginTx(t)
	if err := service.InheritProgressFollows(t.Context(), q, parent, child); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := followAddManual(t.Context(), lane, parent); done <- err }()
	if !followBlocked(done) {
		t.Fatal("a follow of the parent committed between a child's enumeration and the child's commit")
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	followFinished(t, "follow after the child create committed", done)
}

// AC1 / M11, subscription side: a child create that starts while a follow of
// its parent is uncommitted waits for it and then inherits it.
func TestDMProgressFollow_ChildCreateWaitsForAnOpenFollowAndInheritsIt(t *testing.T) {
	w := newDMWorld(t)
	lane := w.dm(t, testUserID)
	parent := followIssueOf(t, dbfx.Issue(t, "race parent "+t.Name()))
	child := followIssueOf(t, dbfx.Issue(t, "race child "+t.Name()))

	ftx, fq := followBeginTx(t)
	if _, _, err := service.AddProgressFollow(t.Context(), fq, service.DMLane{Lineage: uuidOf(lane), Owner: uuidOf(testUserID)}, parent, []string{service.ReasonManual}, parent.ID, pgtype.Timestamptz{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		tx, err := testPool.Begin(t.Context())
		if err != nil {
			done <- err
			return
		}
		defer tx.Rollback(context.Background())
		if err := service.InheritProgressFollows(t.Context(), testHandler.Queries.WithTx(tx), parent, child); err != nil {
			done <- err
			return
		}
		done <- tx.Commit(t.Context())
	}()
	if !followBlocked(done) {
		t.Fatal("a child create enumerated the parent's follows while a follow of the parent was still open")
	}
	if err := ftx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	followFinished(t, "child create after the follow committed", done)
	if got := followState(t, uuidToString(child.ID), lane); got != "active" {
		t.Fatalf("child follow = %q, want active (inherited)", got)
	}
}

// AC1: parallel child creation and follow creation lose no child. A child
// whose commit starts after the follow committed must carry the follow.
func TestDMProgressFollow_ParallelChildCreatesAndFollowLoseNoChild(t *testing.T) {
	w := newDMWorld(t)
	lane := w.dm(t, testUserID)
	parent := followIssueOf(t, dbfx.Issue(t, "race parent "+t.Name()))
	const children = 24
	kids := make([]db.Issue, children)
	for i := range kids {
		kids[i] = followIssueOf(t, dbfx.Issue(t, "race child "+t.Name()))
	}

	var followedAt atomic.Int64 // unix nanos after the follow committed; 0 until then
	var wg sync.WaitGroup
	start := make(chan struct{})
	commitStart := make([]int64, children)
	errs := make(chan error, children+1)
	for i := range kids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(time.Duration(i) * 3 * time.Millisecond)
			tx, err := testPool.Begin(t.Context())
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(context.Background())
			if err := service.InheritProgressFollows(t.Context(), testHandler.Queries.WithTx(tx), parent, kids[i]); err != nil {
				errs <- err
				return
			}
			commitStart[i] = time.Now().UnixNano()
			errs <- tx.Commit(t.Context())
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(30 * time.Millisecond)
		_, err := followAddManual(t.Context(), lane, parent)
		followedAt.Store(time.Now().UnixNano())
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, k := range kids {
		if commitStart[i] > followedAt.Load() && followState(t, uuidToString(k.ID), lane) != "active" {
			t.Errorf("child %d committed after the parent's follow committed but is not followed", i)
		}
	}
}

// AC2: a new follow's snapshot is read under the issue lock, so a status change
// that commits before the follow is in the snapshot (no event) and every later
// one is in the outbox (one event): no gap, no duplicate.
func TestDMProgressFollow_SnapshotAndOutboxSplitAtTheWatermark(t *testing.T) {
	w := newDMWorld(t)
	lane := w.dm(t, testUserID)
	id := dbfx.Issue(t, "watermark "+t.Name())
	stale := followIssueOf(t, id) // what a caller read before the status change

	tx, q := followBeginTx(t)
	if _, err := q.UpdateIssueStatus(t.Context(), db.UpdateIssueStatusParams{ID: stale.ID, Status: "in_progress", WorkspaceID: stale.WorkspaceID}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		f   db.DmProgressFollow
		err error
	}
	done := make(chan result, 1)
	go func() { f, err := followAddManual(t.Context(), lane, stale); done <- result{f, err} }()
	select {
	case <-done:
		t.Fatal("a follow was taken while the issue had an uncommitted status change")
	case <-time.After(400 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(followRaceWait):
		t.Fatal("follow did not finish after the status change committed")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.f.LastRenderedStatus != "in_progress" {
		t.Errorf("snapshot status = %q, want in_progress (the change committed before the follow)", r.f.LastRenderedStatus)
	}
	if n := len(pendingRows(t, id)); n != 0 {
		t.Errorf("pending rows = %d, want 0: the earlier change lives in the snapshot, not the outbox", n)
	}

	if _, err := testHandler.Queries.UpdateIssueStatus(t.Context(), db.UpdateIssueStatusParams{ID: stale.ID, Status: "done", WorkspaceID: stale.WorkspaceID}); err != nil {
		t.Fatal(err)
	}
	wantPending(t, id, lane, "status")
}

// AC2: a re-follow takes a new snapshot, so events the old follow captured and
// nobody sent are not replayed after it; events after the watermark are.
func TestDMProgressFollow_RefollowDropsEventsBeforeItsWatermark(t *testing.T) {
	e := newDispatchEnv(t)
	q := testHandler.Queries
	assign := func() {
		dbfx.Exec(t, `UPDATE issue SET assignee_type = 'member', assignee_id = $2 WHERE id = $1`, e.issue, testUserID)
	}
	assign() // captured for the active follow, never sent
	if n := len(pendingRows(t, e.issue)); n != 1 {
		t.Fatalf("pending after the assignee change = %d, want 1", n)
	}
	if _, err := q.UnfollowDMProgressFollow(t.Context(), db.UnfollowDMProgressFollowParams{IssueID: uuidOf(e.issue), LineageID: uuidOf(e.lane)}); err != nil {
		t.Fatal(err)
	}
	if _, err := followAddManual(t.Context(), e.lane, followIssueOf(t, e.issue)); err != nil {
		t.Fatal(err)
	}

	later := time.Now().Add(time.Hour)
	if err := e.d.Run(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	e.wantCards(t, 0)
	if n := e.pending(t); n != 0 {
		t.Errorf("pending = %d, want the pre-watermark row dropped", n)
	}

	dbfx.Exec(t, `UPDATE issue SET assignee_id = NULL, assignee_type = NULL WHERE id = $1`, e.issue)
	if err := e.d.Run(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	e.wantCards(t, 1)
}
