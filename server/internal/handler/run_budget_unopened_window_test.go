package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A window nobody opened still holds credit or debt: the runs counted in it
// against the cap. A top-up or a person's status move starts the next window,
// and the old balance has to be written first, because a ledger sum cannot
// recover an entry that was never written. Each case admits a run with no
// pre-opening call, so the first ledger write is the one under test.

func TestRunBudgetTopUpKeepsTheCreditOfAnUnopenedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)

	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	admitRun(t, c)

	// 2 runs left in the old window + 40 in the new one, minus the admitted run.
	if got := ledgerBalance(t, c.issueID); got != 41 {
		t.Fatalf("balance after top-up and one admission = %d, want 41 (42 before the run)", got)
	}
}

func TestRunBudgetTopUpKeepsTheDebtOfAnUnopenedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 43)

	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	admitRun(t, c)

	// 3 runs overspent in the old window take 3 off the new window's 40.
	if got := ledgerBalance(t, c.issueID); got != 36 {
		t.Fatalf("balance after top-up and one admission = %d, want 36 (37 before the run)", got)
	}
}

func TestRunBudgetMemberStatusMoveKeepsTheCreditOfAnUnopenedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	recordMemberMovesOf(t, c.issueID)

	moveStatusAsMember(t, c.issueID, "in_review")
	admitRun(t, c)

	if got := ledgerBalance(t, c.issueID); got != 41 {
		t.Fatalf("balance after a member move and one admission = %d, want 41 (42 before the run)", got)
	}
}

func TestRunBudgetMemberStatusMoveKeepsTheDebtOfAnUnopenedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 43)
	recordMemberMovesOf(t, c.issueID)

	moveStatusAsMember(t, c.issueID, "in_review")
	admitRun(t, c)

	if got := ledgerBalance(t, c.issueID); got != 36 {
		t.Fatalf("balance after a member move and one admission = %d, want 36 (37 before the run)", got)
	}
}

// holdRunBudgetBoundary takes the issue's boundary lock the way a move in
// flight holds it, until the returned release runs.
func holdRunBudgetBoundary(t *testing.T, issueID string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := testHandler.Queries.WithTx(tx).LockIssueRunBudgetBoundary(ctx, parseUUID(issueID)); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	return func() { tx.Rollback(ctx) }
}

// stillRunning reports whether done stays open through a short wait.
func stillRunning(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	case <-time.After(400 * time.Millisecond):
		return true
	}
}

func TestRunBudgetTopUpWaitsForAMoveInFlight(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	release := holdRunBudgetBoundary(t, c.issueID)
	defer release()

	done := make(chan struct{})
	var code int
	go func() {
		defer close(done)
		code = topUpBudget(t, c.issueID, nil).Code
	}()
	if !stillRunning(done) {
		t.Fatal("the top-up finished while a window boundary was in flight")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up'`, c.issueID); n != 0 {
		t.Fatalf("top-up activity rows while the boundary was held = %d, want 0", n)
	}
	release()
	<-done
	if code != http.StatusOK {
		t.Fatalf("top-up = %d", code)
	}
	admitRun(t, c)
	if got := ledgerBalance(t, c.issueID); got != 41 {
		t.Fatalf("balance after the top-up and one admission = %d, want 41", got)
	}
}

func TestRunBudgetMemberMoveWaitsForAMoveInFlight(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	recordMemberMovesOf(t, c.issueID)
	release := holdRunBudgetBoundary(t, c.issueID)
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		moveStatusAsMember(t, c.issueID, "in_review")
	}()
	if !stillRunning(done) {
		t.Fatal("the move finished while a window boundary was in flight")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id=$1 AND status='in_review'`, c.issueID); n != 0 {
		t.Fatal("the move was written while a window boundary was in flight")
	}
	release()
	<-done
	admitRun(t, c)
	if got := ledgerBalance(t, c.issueID); got != 41 {
		t.Fatalf("balance after the move and one admission = %d, want 41", got)
	}
}

// failFromBegin passes the first n-1 transactions to the real pool and refuses
// every later one.
type failFromBegin struct {
	real  txStarter
	n     int32
	count atomic.Int32
}

func (f *failFromBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	if f.count.Add(1) >= f.n {
		return nil, errors.New("begin refused")
	}
	return f.real.Begin(ctx)
}

// A move whose old window cannot be written is refused before it changes the
// issue: writing it anyway would start the next window over an unwritten one.
func TestRunBudgetMemberMoveFailsWhenTheOldWindowCannotBeWritten(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	recordMemberMovesOf(t, c.issueID)

	real := testHandler.TxStarter
	// The first transaction takes the boundary lock; the second would write the window.
	testHandler.TxStarter = &failFromBegin{real: real, n: 2}
	req := memberMoveRequest(c.issueID)
	w := httptest.NewRecorder()
	testHandler.UpdateIssue(w, req)
	testHandler.TxStarter = real

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("move with a failing ledger write = %d, want 500: %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id=$1 AND status='in_progress'`, c.issueID); n != 1 {
		t.Fatal("the refused move still changed the issue status")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='status_changed'`, c.issueID); n != 0 {
		t.Fatalf("status_changed activity rows after a refused move = %d, want 0", n)
	}

	// The lock was released and nothing was lost: the same move now goes through.
	moveStatusAsMember(t, c.issueID, "in_review")
	admitRun(t, c)
	if got := ledgerBalance(t, c.issueID); got != 41 {
		t.Fatalf("balance after the retried move and one admission = %d, want 41", got)
	}
}

// A request that asks for the status the issue has when it reads it is not yet
// known to be a no-op: another move may change the status while it waits for
// the boundary lock, and it then moves the issue back, which ends a window.
func TestRunBudgetSameStatusRequestChecksTheStatusUnderTheLock(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	recordMemberMovesOf(t, c.issueID)
	release := holdRunBudgetBoundary(t, c.issueID)
	defer release()

	done := make(chan struct{})
	var code int
	go func() {
		defer close(done)
		req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+c.issueID, map[string]any{"status": "in_progress", "suppress_run": true}), "id", c.issueID)
		req.Header.Set("X-Actor-Source", "interactive_jwt")
		w := httptest.NewRecorder()
		testHandler.UpdateIssue(w, req)
		code = w.Code
	}()
	if !stillRunning(done) {
		t.Fatal("the request finished while a window boundary was in flight")
	}
	// The move that held the lock changed the status before the request got in.
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='in_review' WHERE id=$1`, c.issueID); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	if code != http.StatusOK {
		t.Fatalf("request = %d", code)
	}
	admitRun(t, c)
	if got := ledgerBalance(t, c.issueID); got != 41 {
		t.Fatalf("balance after the request and one admission = %d, want 41 (the old window's 2 runs kept)", got)
	}
}

// A top-up that waits for the boundary lock starts its window when it gets the
// lock, not when its transaction began: a move recorded while it waited must
// not look newer than the top-up.
func TestRunBudgetTopUpStartsItsWindowAfterAMoveItWaitedFor(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	release := holdRunBudgetBoundary(t, c.issueID)
	defer release()

	done := make(chan struct{})
	var code int
	go func() {
		defer close(done)
		code = topUpBudget(t, c.issueID, nil).Code
	}()
	if !stillRunning(done) {
		t.Fatal("the top-up finished while a window boundary was in flight")
	}
	// The move holding the lock records its boundary after the top-up began.
	if _, err := testPool.Exec(context.Background(), `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		SELECT workspace_id, id, 'member', $2::uuid, 'status_changed', '{}' FROM issue WHERE id=$1`, c.issueID, testUserID); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	if code != http.StatusOK {
		t.Fatalf("top-up = %d", code)
	}
	var latest string
	dbfx.QueryRow(t, `SELECT action FROM activity_log WHERE issue_id=$1 AND action IN ('status_changed','run_budget_top_up') AND actor_type='member'
		ORDER BY created_at DESC LIMIT 1`, c.issueID).Scan(&latest)
	if latest != "run_budget_top_up" {
		t.Fatalf("latest window boundary = %q, want the top-up", latest)
	}
}

// A person's request reads the status under the boundary lock, so an agent's
// or the platform's status write has to wait for that lock: one that landed
// between the read and the person's own write could be undone by it with no
// window written for the status it set.
func TestRunBudgetAgentStatusWriteWaitsForAMoveInFlight(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c := newEdgeCase(t, 2885901, recoverySteps(seededReadyAgentID(t)))
	spendBudget(t, c, 38, "in_progress")
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_workflow SET max_runs_per_issue=40 WHERE id=$1`, c.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `DELETE FROM issue_run_budget_entry WHERE issue_id=$1`, c.issueID); err != nil {
		t.Fatal(err)
	}
	as := agentCaller(t, c, false)
	release := holdRunBudgetBoundary(t, c.issueID)
	defer release()

	done := make(chan struct{})
	var code int
	var body string
	go func() {
		defer close(done)
		req := as(withURLParam(newRequest(http.MethodPut, "/api/issues/"+c.issueID, map[string]any{"status": "in_review"}), "id", c.issueID))
		w := httptest.NewRecorder()
		testHandler.UpdateIssue(w, req)
		code = w.Code
		body = w.Body.String()
	}()
	if !stillRunning(done) {
		t.Fatalf("the agent's write finished while a window boundary was in flight (code %d: %s)", code, body)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id=$1 AND status='in_review'`, c.issueID); n != 0 {
		t.Fatal("the agent's write landed while a window boundary was in flight")
	}
	release()
	<-done
	if code != http.StatusOK {
		t.Fatalf("agent write = %d: %s", code, body)
	}
}

func TestRunBudgetWorkflowEdgeMoveWaitsForAMoveInFlight(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	release := holdRunBudgetBoundary(t, c.issueID)
	defer release()

	done := make(chan struct{})
	var moved bool
	go func() {
		defer close(done)
		moved, _ = moveIssueOnEdgeOf(c.issueID, "in_review")
	}()
	if !stillRunning(done) {
		t.Fatal("the edge move finished while a window boundary was in flight")
	}
	release()
	<-done
	if !moved {
		t.Fatal("the edge move did not happen after the boundary was released")
	}
}

// The block an exhausted budget causes runs under the locks of the admission
// that was refused, and that admission has already written the window's
// ledger row. Taking the boundary lock there inverts the order a person's move
// uses (boundary lock first), which deadlocked concurrent refusals.
func TestRunBudgetExhaustionBlockDoesNotWaitForTheBoundaryLock(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	release := holdRunBudgetBoundary(t, c.issueID)
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		testHandler.moveIssueOnEdge(context.Background(), issue, "blocked", "run_budget", nil, false)
	}()
	if stillRunning(done) {
		t.Fatal("the exhaustion block waited for the window boundary lock")
	}
}

// moveIssueOnEdgeOf moves the issue as a workflow edge does, reading the issue
// again when a concurrent writer wins the revision: the notice an admission
// posts in the background also writes the issue, and it lands at any moment
// after the admission, so one read of the issue can be stale by the move.
func moveIssueOnEdgeOf(issueID, to string) (bool, error) {
	return moveIssueOnEdgeWith(context.Background(), testHandler, issueID, to)
}

func moveIssueOnEdgeWith(ctx context.Context, h *Handler, issueID, to string) (bool, error) {
	var err error
	for range 20 {
		var issue db.Issue
		if issue, err = h.Queries.GetIssue(ctx, parseUUID(issueID)); err != nil {
			return false, err
		}
		var moved bool
		if moved, _, err = h.tryMoveIssueOnEdge(ctx, issue, to, "ci_green_advance", nil, false); moved || !errors.Is(err, pgx.ErrNoRows) {
			return moved, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, err
}

// withSmallPool returns a copy of the test handler on a pool of n connections:
// a lock holder that needs a connection must still find one. The shared
// handler stays untouched, because goroutines of earlier tests still read it.
func withSmallPool(t *testing.T, n int32) *Handler {
	t.Helper()
	cfg := testPool.Config().Copy()
	cfg.MaxConns = n
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	h := *testHandler
	h.Queries, h.TxStarter, h.DB = db.New(small), small, small
	return &h
}

// requireAllFinish runs the requests at once and fails unless each returns 200
// within the time the request context allows.
func requireAllFinish(t *testing.T, what string, reqs []*http.Request, serve http.HandlerFunc) {
	t.Helper()
	codes := make([]int, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
			defer cancel()
			w := httptest.NewRecorder()
			serve(w, req.WithContext(ctx))
			codes[i] = w.Code
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200: %d concurrent %s on a pool of 4 connections must all finish", i, code, len(reqs), what)
		}
	}
}

func memberMoveRequest(issueID string) *http.Request {
	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"status": "in_review", "suppress_run": true}), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	return req
}

// Many requests for one issue queue on its boundary lock. A request that
// waits holds no pooled connection, so the one that holds the lock can still
// get the connections it needs to finish, even when the pool is small.
func TestRunBudgetBoundaryWaitersDoNotExhaustTheConnectionPool(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	recordMemberMovesOf(t, c.issueID)
	h := withSmallPool(t, 4)

	reqs := make([]*http.Request, 20)
	for i := range reqs {
		reqs[i] = memberMoveRequest(c.issueID)
	}
	requireAllFinish(t, "moves on one issue", reqs, h.UpdateIssue)
}

// Lock holders of different issues each hold a connection and need another to
// finish, so the number of holders is bounded below the pool size.
func TestRunBudgetLockHoldersOfDifferentIssuesDoNotExhaustTheConnectionPool(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	reqs := make([]*http.Request, 12)
	for i := range reqs {
		c, _ := budgetedAt(t, 40, 38)
		recordMemberMovesOf(t, c.issueID)
		reqs[i] = memberMoveRequest(c.issueID)
	}
	h := withSmallPool(t, 4)
	requireAllFinish(t, "moves on different issues", reqs, h.UpdateIssue)
}

// An edge move that nothing started inside another request's lock (a CI
// advance, the idle guard, a due workflow edge) is one more lock holder, so it
// counts against the pool like a person's move does.
func TestRunBudgetIndependentEdgeMovesOfDifferentIssuesDoNotExhaustTheConnectionPool(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ids := make([]string, 12)
	for i := range ids {
		c, _ := budgetedAt(t, 40, 38)
		ids[i] = c.issueID
	}
	h := withSmallPool(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	moved := make([]bool, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			moved[i], errs[i] = moveIssueOnEdgeWith(ctx, h, id, "in_review")
		}()
	}
	wg.Wait()
	for i := range ids {
		if !moved[i] {
			t.Fatalf("edge move %d did not happen: %v; %d concurrent edge moves on different issues on a pool of 4 connections must all finish", i, errs[i], len(ids))
		}
	}
}

// A top-up waits for the boundary lock like a move does, so it queues in the
// same order and does not hold a connection while it waits. A pool of two
// connections makes the order deterministic: another holder has the lock, a
// move arrives and then a top-up. If the top-up held the second connection
// while it waited, the move would get the lock and then find no connection.
func TestRunBudgetATopUpBehindAMoveDoesNotHoldTheMovesConnection(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	recordMemberMovesOf(t, c.issueID)
	h := withSmallPool(t, 2)
	release := holdRunBudgetBoundary(t, c.issueID)
	defer release()

	within := func(req *http.Request) *http.Request {
		ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
		t.Cleanup(cancel)
		return req.WithContext(ctx)
	}
	moveDone, topUpDone := make(chan struct{}), make(chan struct{})
	moved, toppedUp := httptest.NewRecorder(), httptest.NewRecorder()
	go func() {
		defer close(moveDone)
		h.UpdateIssue(moved, within(memberMoveRequest(c.issueID)))
	}()
	if !stillRunning(moveDone) {
		t.Fatal("the move finished while another request held the boundary lock")
	}
	go func() {
		defer close(topUpDone)
		req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+c.issueID+"/run-budget/top-up", nil), "id", c.issueID)
		h.TopUpIssueRunBudget(toppedUp, within(req))
	}()
	if !stillRunning(topUpDone) {
		t.Fatal("the top-up finished while another request held the boundary lock")
	}
	release()
	<-moveDone
	<-topUpDone
	if moved.Code != http.StatusOK || toppedUp.Code != http.StatusOK {
		t.Fatalf("move = %d, top-up = %d, want 200 and 200: %s %s", moved.Code, toppedUp.Code, moved.Body.String(), toppedUp.Body.String())
	}
}
