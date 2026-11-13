package ciwatch

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Needs a migrated database: DATABASE_URL=... go test ./internal/ciwatch -run Ledger

type actClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *actClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *actClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type actEnv struct {
	pool    *pgxpool.Pool
	clock   *actClock
	ledger  *Ledger
	ws      string
	repo    string
	verdict atomic.Value // Verdict returned by the recheck
	effects atomic.Int32
}

func newLedgerEnv(t *testing.T) *actEnv {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	e := &actEnv{pool: pool, clock: &actClock{t: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)},
		ws: uuid.NewString(), repo: "r" + uuid.NewString()[:8]}
	e.verdict.Store(Verdict{})
	e.ledger = NewLedger(pool, func(context.Context, pgx.Tx, Action) (Verdict, error) {
		return e.verdict.Load().(Verdict), nil
	}, WithLedgerClock(e.clock.Now))
	// A private schema: the workspace-deletion manifest test classifies every public table.
	if _, err = pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS ciwatch_test`); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS ciwatch_test.effect (kind text NOT NULL, ref text NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM ci_watch_action WHERE workspace_id=$1`, e.ws)
		_, _ = pool.Exec(ctx, `DELETE FROM ci_signature_owner WHERE workspace_id=$1`, e.ws)
		_, _ = pool.Exec(ctx, `DELETE FROM ciwatch_test.effect WHERE ref LIKE $1`, e.ws+"%")
		pool.Close()
	})
	return e
}

func (e *actEnv) action(runID int64, kind, sig string) NewAction {
	return NewAction{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, OriginalRunID: runID,
		Kind: kind, Protocol: "p1", SignatureHash: sig, DueAt: e.clock.Now(), RunCompletedAt: e.clock.Now()}
}

func (e *actEnv) count(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ciwatch_test.effect WHERE kind=$1 AND ref LIKE $2`, kind, e.ws+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *actEnv) get(t *testing.T, id string) Action {
	t.Helper()
	a, err := e.ledger.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// insertEffect writes the destination row (issue, comment, notification) and
// the owner wake in the caller's transaction, as the real executors do.
func (e *actEnv) insertEffect(kind string) LocalEffect {
	return func(ctx context.Context, tx pgx.Tx, a Action) (string, error) {
		e.effects.Add(1)
		dest := uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO ciwatch_test.effect (kind, ref) VALUES ($1, $2)`, kind, e.ws+"/"+dest); err != nil {
			return "", err
		}
		if kind == "issue" {
			if _, err := tx.Exec(ctx, `INSERT INTO ciwatch_test.effect (kind, ref) VALUES ('wake', $1)`, e.ws+"/"+dest); err != nil {
				return "", err
			}
		}
		return dest, nil
	}
}

func TestLedgerConcurrentScheduleClaimsOnce(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	var inserted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ins, err := e.ledger.Schedule(ctx, e.action(42, ActionRerun, ""))
			if err != nil {
				t.Error(err)
			}
			if ins {
				inserted.Add(1)
			}
		}()
	}
	wg.Wait()
	if inserted.Load() != 1 {
		t.Fatalf("inserted %d times, want 1", inserted.Load())
	}
	// A second attempt of the same original run finds the same row.
	a, ins, err := e.ledger.Schedule(ctx, e.action(42, ActionRerun, ""))
	if err != nil || ins {
		t.Fatalf("second attempt: inserted=%v err=%v", ins, err)
	}
	if a.State != StateScheduled {
		t.Fatalf("state = %s", a.State)
	}
}

func TestLedgerLocalEffectRunsOnceAcrossWorkersAndRestarts(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	e.ledger.RegisterLocal(ActionIssueComment, e.insertEffect("comment"))
	a, _, _ := e.ledger.Schedule(ctx, e.action(1, ActionIssueComment, ""))

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.ledger.RunOne(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// A lost response or a restart: a fresh ledger on the same database finds nothing to do.
	e2 := NewLedger(e.pool, nil, WithLedgerClock(e.clock.Now))
	e2.RegisterLocal(ActionIssueComment, e.insertEffect("comment"))
	ran, err := e2.RunOne(ctx)
	if err != nil || ran {
		t.Fatalf("after restart: ran=%v err=%v", ran, err)
	}
	if got := e.count(t, "comment"); got != 1 {
		t.Fatalf("comments = %d, want 1", got)
	}
	done := e.get(t, a.ID)
	if done.State != StateDone || done.DestinationID == "" || done.LeaseEpoch != 1 {
		t.Fatalf("action = %+v", done)
	}
}

// Worker A's insert is held open past lease expiry while a recovery sweep and
// another worker run; A then commits. One effect per kind.
func TestLedgerDelayedInsertRace(t *testing.T) {
	for _, tc := range []struct{ kind, effect string }{
		{ActionIssueCreate, "issue"}, {ActionIssueComment, "comment"}, {ActionInboxNotify, "notification"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			e := newLedgerEnv(t)
			ctx := context.Background()
			sig := ""
			if tc.kind == ActionIssueCreate {
				sig = "sig-" + e.repo
			}
			inserted, release := make(chan struct{}), make(chan struct{})
			base := e.insertEffect(tc.effect)
			e.ledger.RegisterLocal(tc.kind, func(ctx context.Context, tx pgx.Tx, a Action) (string, error) {
				id, err := base(ctx, tx, a)
				close(inserted)
				<-release
				return id, err
			})
			a := e.schedule(t, 7, tc.kind, sig)

			var wgA sync.WaitGroup
			wgA.Add(1)
			go func() {
				defer wgA.Done()
				if _, err := e.ledger.RunOne(ctx); err != nil {
					t.Errorf("worker A: %v", err)
				}
			}()
			<-inserted
			e.clock.Advance(10 * time.Minute) // lease long expired
			if n, err := e.ledger.Sweep(ctx); err != nil || n != 0 {
				t.Fatalf("sweep: n=%d err=%v", n, err)
			}
			if ran, err := e.ledger.RunOne(ctx); err != nil || ran {
				t.Fatalf("worker B: ran=%v err=%v", ran, err)
			}
			close(release)
			wgA.Wait()

			if got := e.count(t, tc.effect); got != 1 {
				t.Fatalf("%s rows = %d, want 1", tc.effect, got)
			}
			if tc.kind == ActionIssueCreate {
				if got := e.count(t, "wake"); got != 1 {
					t.Fatalf("wakes = %d, want 1", got)
				}
				var state string
				var issue *string
				if err := e.pool.QueryRow(ctx, `SELECT state, issue_id::text FROM ci_signature_owner WHERE workspace_id=$1 AND signature_hash=$2`, e.ws, sig).Scan(&state, &issue); err != nil {
					t.Fatal(err)
				}
				done := e.get(t, a.ID)
				if state != "open" || issue == nil || *issue != done.DestinationID {
					t.Fatalf("owner state=%s issue=%v action dest=%s", state, issue, done.DestinationID)
				}
			}
			if st := e.get(t, a.ID).State; st != StateDone {
				t.Fatalf("state = %s", st)
			}
		})
	}
}

func TestLedgerSweepRecoversUnlockedExecutingRows(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	e.ledger.RegisterLocal(ActionInboxNotify, e.insertEffect("notification"))
	a, _, _ := e.ledger.Schedule(ctx, e.action(8, ActionInboxNotify, ""))
	if _, err := e.pool.Exec(ctx, `UPDATE ci_watch_action SET state='executing', lease_epoch=3, lease_until=$2 WHERE id=$1`,
		a.ID, e.clock.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.ledger.Sweep(ctx); n != 0 {
		t.Fatalf("swept %d before lease expiry", n)
	}
	e.clock.Advance(3 * time.Minute)
	if n, err := e.ledger.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
	got := e.get(t, a.ID)
	if got.State != StateScheduled || got.LeaseEpoch != 4 {
		t.Fatalf("after sweep: %s epoch %d", got.State, got.LeaseEpoch)
	}
	if _, err := e.ledger.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, "notification") != 1 {
		t.Fatal("want exactly one notification")
	}
}

func TestLedgerKilledWorkerRollsBackThenOneEffect(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	var first atomic.Bool
	base := e.insertEffect("comment")
	e.ledger.RegisterLocal(ActionIssueComment, func(ctx context.Context, tx pgx.Tx, a Action) (string, error) {
		id, err := base(ctx, tx, a)
		if err == nil && first.CompareAndSwap(false, true) {
			var pid int
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return "", err
			}
			if _, err := e.pool.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
				return "", err
			}
			time.Sleep(200 * time.Millisecond)
			_, err = tx.Exec(ctx, `SELECT 1`) // the connection is gone
		}
		return id, err
	})
	a, _, _ := e.ledger.Schedule(ctx, e.action(9, ActionIssueComment, ""))
	if _, err := e.ledger.RunOne(ctx); err == nil {
		t.Fatal("expected the killed worker to fail")
	}
	if e.count(t, "comment") != 0 {
		t.Fatal("killed transaction left a comment")
	}
	got := e.get(t, a.ID)
	if got.State != StateScheduled || got.Attempts != 1 {
		t.Fatalf("after kill: %s attempts %d", got.State, got.Attempts)
	}
	e.clock.Advance(time.Hour)
	if ran, err := e.ledger.RunOne(ctx); err != nil || !ran {
		t.Fatalf("retry: ran=%v err=%v", ran, err)
	}
	if e.count(t, "comment") != 1 {
		t.Fatal("want exactly one comment after the retry")
	}
}

func TestLedgerRolledBackEffectIsBoundedAndPermanentErrorsStop(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	e.ledger.RegisterLocal(ActionIssueComment, func(context.Context, pgx.Tx, Action) (string, error) {
		return "", errors.New("boom")
	})
	e.ledger.RegisterLocal(ActionInboxNotify, func(context.Context, pgx.Tx, Action) (string, error) {
		return "", &PermanentError{Reason: "non_transactional_destination", Err: errors.New("no tx path")}
	})
	a, _, _ := e.ledger.Schedule(ctx, e.action(10, ActionIssueComment, ""))
	b, _, _ := e.ledger.Schedule(ctx, e.action(11, ActionInboxNotify, ""))
	for i := 0; i < MaxAttempts+2; i++ {
		_, _ = e.ledger.RunOne(ctx)
		e.clock.Advance(time.Hour)
	}
	if got := e.get(t, a.ID); got.State != StateFailed || got.Reason != "attempts_exhausted" || got.Attempts != MaxAttempts {
		t.Fatalf("bounded: %+v", got)
	}
	if got := e.get(t, b.ID); got.State != StateFailed || got.Reason != "non_transactional_destination" {
		t.Fatalf("permanent: %+v", got)
	}
}

func TestLedgerRecheckCancelsDefersAndGoesStale(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	e.ledger.RegisterLocal(ActionIssueComment, e.insertEffect("comment"))

	// cancel at recheck (manual rerun, breaker, cap ...)
	a, _, _ := e.ledger.Schedule(ctx, e.action(20, ActionIssueComment, ""))
	e.verdict.Store(Verdict{CancelReason: ReasonManualRerun})
	if ran, err := e.ledger.RunOne(ctx); err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if got := e.get(t, a.ID); got.State != StateCancelled || got.Reason != ReasonManualRerun {
		t.Fatalf("cancel: %+v", got)
	}

	// budget deferral keeps scheduled and moves due_at
	b, _, _ := e.ledger.Schedule(ctx, e.action(21, ActionIssueComment, ""))
	reset := e.clock.Now().Add(40 * time.Minute)
	e.verdict.Store(Verdict{DeferUntil: reset})
	_, _ = e.ledger.RunOne(ctx)
	got := e.get(t, b.ID)
	if got.State != StateScheduled || !got.DueAt.Equal(reset) || got.LeaseEpoch != 0 {
		t.Fatalf("defer: %+v", got)
	}
	if ran, _ := e.ledger.RunOne(ctx); ran {
		t.Fatal("ran before the reset")
	}
	e.verdict.Store(Verdict{})
	e.clock.Advance(41 * time.Minute)
	if ran, err := e.ledger.RunOne(ctx); err != nil || !ran || e.count(t, "comment") != 1 {
		t.Fatalf("after reset: ran=%v err=%v comments=%d", ran, err, e.count(t, "comment"))
	}

	// a rerun still scheduled 6 h after its run completed is stale
	e.ledger.RegisterRemote(ActionRerun, &fakeRemote{})
	c, _, _ := e.ledger.Schedule(ctx, e.action(22, ActionRerun, ""))
	e.clock.Advance(StaleAfter + time.Minute)
	_, _ = e.ledger.RunOne(ctx)
	if got := e.get(t, c.ID); got.State != StateCancelled || got.Reason != ReasonStale {
		t.Fatalf("stale: %+v", got)
	}
}

func TestLedgerCancelScheduledOnOptOut(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	e.ledger.RegisterLocal(ActionIssueComment, e.insertEffect("comment"))
	delayed := e.action(30, ActionIssueComment, "")
	delayed.DueAt = e.clock.Now().Add(2 * time.Minute)
	a, _, _ := e.ledger.Schedule(ctx, delayed)
	other := e.action(31, ActionIssueComment, "")
	other.Protocol = "p2"
	o, _, _ := e.ledger.Schedule(ctx, other)

	n, err := e.ledger.CancelScheduled(ctx, CancelFilter{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, Protocol: "p1"}, ReasonOptedOut)
	if err != nil || n != 1 {
		t.Fatalf("cancelled %d err %v", n, err)
	}
	e.clock.Advance(5 * time.Minute)
	_, _ = e.ledger.RunOne(ctx)
	if got := e.get(t, a.ID); got.State != StateCancelled || got.Reason != ReasonOptedOut {
		t.Fatalf("opt-out: %+v", got)
	}
	if got := e.get(t, o.ID); got.State != StateDone {
		t.Fatalf("other protocol: %+v", got)
	}
	if e.count(t, "comment") != 1 {
		t.Fatalf("comments = %d, want 1 (the other protocol only)", e.count(t, "comment"))
	}
}

type fakeRemote struct {
	mu        sync.Mutex
	executes  int
	reconcile int
	outcome   RemoteOutcome
	err       error
	found     []bool // answers of successive Reconcile calls
	reconOut  RemoteOutcome
	markFirst bool
}

func (f *fakeRemote) Execute(ctx context.Context, a Action, mark func(expectedAttempt int) error) (RemoteOutcome, error) {
	f.mu.Lock()
	f.executes++
	f.mu.Unlock()
	if f.markFirst {
		if err := mark(2); err != nil {
			return RemoteOutcome{}, err
		}
	}
	return f.outcome, f.err
}

func (f *fakeRemote) Reconcile(ctx context.Context, a Action) (RemoteOutcome, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconcile++
	if len(f.found) == 0 {
		return RemoteOutcome{}, false, nil
	}
	found := f.found[0]
	f.found = f.found[1:]
	return f.reconOut, found, nil
}

func TestLedgerRemoteActionNeverPostsTwice(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()

	ok := &fakeRemote{outcome: RemoteOutcome{State: StateDone}, markFirst: true}
	e.ledger.RegisterRemote(ActionRerun, ok)
	a, _, _ := e.ledger.Schedule(ctx, e.action(40, ActionRerun, ""))
	_, _ = e.ledger.RunOne(ctx)
	_, _ = e.ledger.RunOne(ctx)
	got := e.get(t, a.ID)
	if got.State != StateDone || ok.executes != 1 || got.ExpectedAttempt == nil || *got.ExpectedAttempt != 2 || got.PostStartedAt == nil {
		t.Fatalf("done: %+v executes=%d", got, ok.executes)
	}

	// 5xx / timeout / no response: ambiguous, never executed again
	bad := &fakeRemote{err: errors.New("503 from github token ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD")}
	e.ledger.RegisterRemote(ActionRerun, bad)
	b, _, _ := e.ledger.Schedule(ctx, e.action(41, ActionRerun, ""))
	_, _ = e.ledger.RunOne(ctx)
	e.clock.Advance(time.Hour)
	_, _ = e.ledger.RunOne(ctx)
	_, _ = e.ledger.Sweep(ctx)
	got = e.get(t, b.ID)
	if got.State != StateAmbiguous || bad.executes != 1 {
		t.Fatalf("ambiguous: %+v executes=%d", got, bad.executes)
	}
	if len(got.Diagnostic) > 200 || strings.Contains(got.Diagnostic, "ghp_abcdefghijkl") {
		t.Fatalf("diagnostic not bounded and redacted: %q", got.Diagnostic)
	}

	// reconcile: not found once, then terminal failed (read only, no POST)
	e.ledger.RegisterRemote(ActionRerun, bad)
	for i := 0; i < 3; i++ {
		e.clock.Advance(3 * time.Minute)
		if _, err := e.ledger.ReconcileOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got = e.get(t, b.ID)
	if got.State != StateFailed || bad.executes != 1 || bad.reconcile != 2 {
		t.Fatalf("reconcile: %+v executes=%d reconciles=%d", got, bad.executes, bad.reconcile)
	}
}

func TestLedgerRestartMidPostBecomesAmbiguousAndReconcilesToDone(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	r := &fakeRemote{found: []bool{true}, reconOut: RemoteOutcome{State: StateDone}}
	e.ledger.RegisterRemote(ActionRerun, r)
	a, _, _ := e.ledger.Schedule(ctx, e.action(50, ActionRerun, ""))
	// the worker committed executing, sent the POST, then died
	if _, err := e.pool.Exec(ctx, `UPDATE ci_watch_action SET state='executing', lease_epoch=1, lease_until=$2 WHERE id=$1`,
		a.ID, e.clock.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(3 * time.Minute)
	if n, err := e.ledger.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
	if got := e.get(t, a.ID); got.State != StateAmbiguous {
		t.Fatalf("state = %s", got.State)
	}
	e.clock.Advance(3 * time.Minute)
	if ran, err := e.ledger.ReconcileOne(ctx); err != nil || !ran {
		t.Fatalf("reconcile ran=%v err=%v", ran, err)
	}
	if got := e.get(t, a.ID); got.State != StateDone || r.executes != 0 {
		t.Fatalf("after reconcile: %s executes=%d", got.State, r.executes)
	}
}

// claim runs ClaimAndSchedule for the run and reports whether it claimed.
func (e *actEnv) claim(t *testing.T, run int64, sig string) (Action, bool) {
	t.Helper()
	a, ok, err := e.ledger.ClaimAndSchedule(context.Background(), e.action(run, ActionIssueCreate, sig), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return a, ok
}

// schedule inserts an action; issue_create goes through ClaimAndSchedule.
func (e *actEnv) schedule(t *testing.T, run int64, kind, sig string) Action {
	t.Helper()
	if kind == ActionIssueCreate {
		a, ok := e.claim(t, run, sig)
		if !ok {
			t.Fatal("claim failed")
		}
		return a
	}
	a, _, err := e.ledger.Schedule(context.Background(), e.action(run, kind, sig))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *actEnv) ownerCount(t *testing.T, sig string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ci_signature_owner WHERE workspace_id=$1 AND signature_hash=$2`, e.ws, sig).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLedgerSignatureOwnerClaim(t *testing.T) {
	e := newLedgerEnv(t)
	sig := "sig-" + e.repo
	var claimers atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(run int64) {
			defer wg.Done()
			if _, ok := e.claim(t, run, sig); ok {
				claimers.Add(1)
			}
		}(int64(100 + i))
	}
	wg.Wait()
	if claimers.Load() != 1 {
		t.Fatalf("claimers = %d, want 1", claimers.Load())
	}
	var actions int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_watch_action WHERE workspace_id=$1`, e.ws).Scan(&actions)
	if actions != 1 {
		t.Fatalf("actions = %d, want 1", actions)
	}

	// cancelling an unrun issue_create releases its creating owner
	ctx := context.Background()
	sig2 := sig + "-2"
	e.schedule(t, 60, ActionIssueCreate, sig2)
	if n, err := e.ledger.CancelScheduled(ctx, CancelFilter{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo}, ReasonOptedOut); err != nil || n != 2 {
		t.Fatalf("cancel n=%d err=%v", n, err)
	}
	if _, ok := e.claim(t, 61, sig2); !ok {
		t.Fatal("claim after release")
	}

	// a closed owner inside the window is attached to; outside it reopens
	if _, err := e.pool.Exec(ctx, `UPDATE ci_signature_owner SET state='closed', closed_at=$3 WHERE workspace_id=$1 AND signature_hash=$2`,
		e.ws, sig2, e.clock.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.claim(t, 62, sig2); ok {
		t.Fatal("claimed a signature closed one hour ago")
	}
	e.clock.Advance(25 * time.Hour)
	if _, ok := e.claim(t, 63, sig2); !ok {
		t.Fatal("reopen after the window should claim")
	}
}

// Duplicate delivery of a run whose issue_create was cancelled (opt-out) and
// released its owner: the insert conflicts, so no new claim may be left behind.
func TestLedgerDuplicateDeliveryAfterCancelLeavesNoStuckOwner(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	sig := "dup-" + uuid.NewString()[:8]
	first := e.schedule(t, 90, ActionIssueCreate, sig)
	if n, err := e.ledger.CancelScheduled(ctx, CancelFilter{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo}, ReasonOptedOut); err != nil || n != 1 {
		t.Fatalf("cancel n=%d err=%v", n, err)
	}
	if got := e.ownerCount(t, sig); got != 0 {
		t.Fatalf("owner after cancel = %d, want 0", got)
	}

	dup, claimed := e.claim(t, 90, sig) // same run delivered again
	if claimed || dup.ID != first.ID || dup.State != StateCancelled {
		t.Fatalf("duplicate: claimed=%v id=%s state=%s, want existing cancelled %s", claimed, dup.ID, dup.State, first.ID)
	}
	if got := e.ownerCount(t, sig); got != 0 {
		t.Fatalf("duplicate delivery left %d owner row(s), want 0", got)
	}
	// a different run of the same signature can still create the issue
	if _, ok := e.claim(t, 91, sig); !ok {
		t.Fatal("later run could not claim the signature")
	}
}

// Concurrent duplicates of the same run: one claim, one action, one owner.
func TestLedgerConcurrentDuplicateClaimAndSchedule(t *testing.T) {
	e := newLedgerEnv(t)
	sig := "cdup-" + uuid.NewString()[:8]
	var claimers atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := e.claim(t, 95, sig); ok {
				claimers.Add(1)
			}
		}()
	}
	wg.Wait()
	var actions int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_watch_action WHERE workspace_id=$1`, e.ws).Scan(&actions)
	if claimers.Load() != 1 || actions != 1 || e.ownerCount(t, sig) != 1 {
		t.Fatalf("claimers=%d actions=%d owners=%d, want 1/1/1", claimers.Load(), actions, e.ownerCount(t, sig))
	}
}

// A crash between the owner claim and the action insert leaves neither.
func TestLedgerOwnerClaimAndScheduleAreAtomic(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	sig := "atomic-" + uuid.NewString()[:8]
	bad := e.action(70, ActionIssueCreate, sig)
	cctx, cancel := context.WithCancel(ctx)
	cancel() // worker dies: every statement fails, the tx cannot commit
	if _, _, err := e.ledger.ClaimAndSchedule(cctx, bad, time.Hour); err == nil {
		t.Fatal("expected an error from a dead context")
	}
	var actions int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM ci_watch_action WHERE workspace_id=$1`, e.ws).Scan(&actions)
	if e.ownerCount(t, sig) != 0 || actions != 0 {
		t.Fatalf("after crash: owners=%d actions=%d, want 0/0", e.ownerCount(t, sig), actions)
	}
	// a mid-transaction failure after the claim: force the insert to fail
	if _, err := e.pool.Exec(ctx, `ALTER TABLE ci_watch_action ADD CONSTRAINT ci_test_fail_insert CHECK (original_run_id <> 71) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(ctx, `ALTER TABLE ci_watch_action DROP CONSTRAINT IF EXISTS ci_test_fail_insert`)
	})
	if _, _, err := e.ledger.ClaimAndSchedule(ctx, e.action(71, ActionIssueCreate, sig), time.Hour); err == nil {
		t.Fatal("expected the insert to fail")
	}
	if e.ownerCount(t, sig) != 0 {
		t.Fatalf("failed insert left %d owner row(s), want 0", e.ownerCount(t, sig))
	}
	if _, err := e.pool.Exec(ctx, `ALTER TABLE ci_watch_action DROP CONSTRAINT ci_test_fail_insert`); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.claim(t, 71, sig); !ok {
		t.Fatal("retry claim failed")
	}
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM ci_watch_action WHERE workspace_id=$1`, e.ws).Scan(&actions)
	if actions != 1 || e.ownerCount(t, sig) != 1 {
		t.Fatalf("actions=%d owners=%d, want 1/1", actions, e.ownerCount(t, sig))
	}
}

// After a POST and a crash, a restarted ledger that sweeps and runs before
// the rerun executor registers must not turn the row back into work.
func TestLedgerRestartBeforeRemoteRegistrationNeverReschedulesRerun(t *testing.T) {
	e := newLedgerEnv(t)
	ctx := context.Background()
	a, _, _ := e.ledger.Schedule(ctx, e.action(80, ActionRerun, ""))
	if _, err := e.pool.Exec(ctx, `UPDATE ci_watch_action SET state='executing', lease_epoch=1, lease_until=$2 WHERE id=$1`,
		a.ID, e.clock.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(3 * time.Minute)
	// restarted process: nothing registered yet
	if _, err := e.ledger.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, a.ID); got.State != StateAmbiguous {
		t.Fatalf("after sweep: %s, want ambiguous", got.State)
	}
	if ran, err := e.ledger.RunOne(ctx); err != nil || ran {
		t.Fatalf("RunOne ran=%v err=%v before registration", ran, err)
	}
	r := &fakeRemote{found: []bool{true}, reconOut: RemoteOutcome{State: StateDone}}
	e.ledger.RegisterRemote(ActionRerun, r)
	e.clock.Advance(3 * time.Minute)
	if _, err := e.ledger.ReconcileOne(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, a.ID); got.State != StateDone || r.executes != 0 {
		t.Fatalf("final: %s executes=%d, want done/0", got.State, r.executes)
	}
}
