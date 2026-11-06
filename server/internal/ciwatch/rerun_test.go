package ciwatch

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/ghbudget"
)

// Needs a migrated database: DATABASE_URL=... go test ./internal/ciwatch -run Rerun

type fakeRerunGH struct {
	mu      sync.Mutex
	posts   atomic.Int32
	gets    atomic.Int32
	attempt int // run_attempt GitHub reports
	post    func() (RerunResponse, error)
	getErr  error
}

func (f *fakeRerunGH) GetRun(context.Context, int64, string, string, int64) (RunState, http.Header, error) {
	f.gets.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return RunState{}, nil, f.getErr
	}
	return RunState{Attempt: f.attempt, Completed: true}, http.Header{}, nil
}

func (f *fakeRerunGH) RerunFailedJobs(context.Context, int64, string, string, int64) (RerunResponse, error) {
	f.posts.Add(1)
	return f.post()
}

func (f *fakeRerunGH) setAttempt(n int) { f.mu.Lock(); f.attempt = n; f.mu.Unlock() }

type rerunEnv struct {
	*ledgerEnv
	gh        *fakeRerunGH
	rr        *Rerunner
	remaining atomic.Int32
	reset     atomic.Int64 // unix seconds
	notified  atomic.Int32
	inst      int64
}

func newRerunEnv(t *testing.T) *rerunEnv {
	t.Helper()
	e := &rerunEnv{ledgerEnv: newLedgerEnv(t), gh: &fakeRerunGH{attempt: 1}, inst: rand.Int64N(1 << 40)}
	e.remaining.Store(4000)
	e.reset.Store(e.clock.Now().Add(time.Hour).Unix())
	budget := ghbudget.New(e.pool, ghbudget.FetcherFunc(func(context.Context, int64) (int, time.Time, error) {
		return int(e.remaining.Load()), time.Unix(e.reset.Load(), 0), nil
	}), ghbudget.WithClock(e.clock.Now), ghbudget.WithJitter(func() time.Duration { return 0 }))
	e.rr = &Rerunner{API: e.gh, Budget: budget, Pool: e.pool, Now: e.clock.Now,
		Installation: func(context.Context, Action) (int64, error) { return e.inst, nil },
		Notify:       func(context.Context, Action, string) error { e.notified.Add(1); return nil }}
	e.ledger.RegisterRemote(ActionRerun, e.rr)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.pool.Exec(ctx, `DELETE FROM gh_budget_reservation WHERE installation_id=$1`, e.inst)
		_, _ = e.pool.Exec(ctx, `DELETE FROM gh_installation_budget WHERE installation_id=$1`, e.inst)
		_, _ = e.pool.Exec(ctx, `DELETE FROM ci_watch_config_audit WHERE workspace_id=$1`, e.ws)
		_, _ = e.pool.Exec(ctx, `DELETE FROM ci_watch_config WHERE workspace_id=$1`, e.ws)
	})
	return e
}

func (e *rerunEnv) schedule(t *testing.T, runID int64) Action {
	t.Helper()
	a, _, err := e.ledger.Schedule(context.Background(), e.action(runID, ActionRerun, ""))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *rerunEnv) runOne(t *testing.T) {
	t.Helper()
	if _, err := e.ledger.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// settleAll drives every recovery path a worker has; none may POST.
func (e *rerunEnv) settleAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		e.clock.Advance(3 * time.Minute)
		_, _ = e.ledger.Sweep(ctx)
		_, _ = e.ledger.ReconcileOne(ctx)
		_, _ = e.ledger.RunOne(ctx)
	}
}

func (e *rerunEnv) wantPosts(t *testing.T, n int32) {
	t.Helper()
	if got := e.gh.posts.Load(); got != n {
		t.Fatalf("POSTs = %d, want %d", got, n)
	}
}

func accepted() (RerunResponse, error) { return RerunResponse{Status: 201}, nil }

func TestRerunSuccessIsDoneWithOnePost(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = accepted
	a := e.schedule(t, 1001)
	e.runOne(t)
	got := e.get(t, a.ID)
	if got.State != StateDone || got.ExpectedAttempt == nil || *got.ExpectedAttempt != 2 || got.PostStartedAt == nil {
		t.Fatalf("done row: %+v", got)
	}
	e.settleAll(t)
	e.wantPosts(t, 1)
}

func TestRerun5xxAfterPossibleRerunReconcilesToDoneWithoutSecondPost(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = func() (RerunResponse, error) { return RerunResponse{Status: 502, Message: "bad gateway"}, nil }
	a := e.schedule(t, 1002)
	e.runOne(t)
	if got := e.get(t, a.ID); got.State != StateAmbiguous || got.ExpectedAttempt == nil || *got.ExpectedAttempt != 2 {
		t.Fatalf("after 502: %+v", got)
	}
	e.gh.setAttempt(2) // the rerun did land
	e.settleAll(t)
	if got := e.get(t, a.ID); got.State != StateDone {
		t.Fatalf("after reconcile: %+v", got)
	}
	e.wantPosts(t, 1)
}

func TestRerunTimeoutThenNotLandedEndsFailedWithOnePost(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = func() (RerunResponse, error) { return RerunResponse{}, context.DeadlineExceeded }
	a := e.schedule(t, 1003)
	e.runOne(t)
	e.settleAll(t)
	if got := e.get(t, a.ID); got.State != StateFailed || got.Reason != ReasonReconcileNotFound {
		t.Fatalf("row: %+v", got)
	}
	e.wantPosts(t, 1)
}

func TestRerunLateLandingAfterFailedStaysOnePost(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = func() (RerunResponse, error) { return RerunResponse{}, errors.New("EOF") }
	a := e.schedule(t, 1004)
	e.runOne(t)
	e.settleAll(t)
	e.gh.setAttempt(2) // lands after the row ended; outcome labelling sees it, the ledger must not react
	e.settleAll(t)
	if got := e.get(t, a.ID); got.State != StateFailed {
		t.Fatalf("row: %+v", got)
	}
	e.wantPosts(t, 1)
}

func TestRerunRestartMidPostReconcilesWithoutPosting(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = accepted
	a := e.schedule(t, 1005)
	ctx := context.Background()
	// worker committed executing and stored expected_attempt, sent the POST, died
	if _, err := e.pool.Exec(ctx, `UPDATE ci_watch_action SET state='executing', lease_epoch=1, expected_attempt=2,
		post_started_at=$2, lease_until=$2 WHERE id=$1`, a.ID, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.gh.setAttempt(2)
	e.settleAll(t)
	if got := e.get(t, a.ID); got.State != StateDone {
		t.Fatalf("row: %+v", got)
	}
	e.wantPosts(t, 0)
}

func TestRerunPermission403SwitchesP1ToShadowAndNotifiesOnce(t *testing.T) {
	e := newRerunEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name, mode, p1_mode)
		VALUES ($1::uuid,'o',$2,'act','act')`, e.ws, e.repo); err != nil {
		t.Fatal(err)
	}
	e.gh.post = func() (RerunResponse, error) {
		return RerunResponse{Status: 403, Message: "Resource not accessible by integration"}, nil
	}
	a := e.schedule(t, 1006)
	e.runOne(t)
	e.settleAll(t)
	var p1 string
	var audits int
	_ = e.pool.QueryRow(ctx, `SELECT p1_mode FROM ci_watch_config WHERE workspace_id=$1::uuid`, e.ws).Scan(&p1)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM ci_watch_config_audit WHERE workspace_id=$1::uuid AND field='p1_mode'
		AND old_value='act' AND new_value='shadow'`, e.ws).Scan(&audits)
	if p1 != "shadow" || audits != 1 || e.notified.Load() != 1 {
		t.Fatalf("p1_mode=%s audits=%d notified=%d", p1, audits, e.notified.Load())
	}
	if got := e.get(t, a.ID); got.State != StateFailed {
		t.Fatalf("row: %+v", got)
	}
	e.wantPosts(t, 1)
}

func TestRerunRateLimit403DoesNotSwitchShadowAndBacksOff(t *testing.T) {
	e := newRerunEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name, mode, p1_mode)
		VALUES ($1::uuid,'o',$2,'act','act')`, e.ws, e.repo); err != nil {
		t.Fatal(err)
	}
	e.gh.post = func() (RerunResponse, error) {
		h := http.Header{"Retry-After": []string{"600"}}
		return RerunResponse{Status: 403, Header: h, Message: "You have exceeded a secondary rate limit"}, nil
	}
	a := e.schedule(t, 1007)
	e.runOne(t)
	var p1 string
	var backoff *time.Time
	_ = e.pool.QueryRow(ctx, `SELECT p1_mode FROM ci_watch_config WHERE workspace_id=$1::uuid`, e.ws).Scan(&p1)
	_ = e.pool.QueryRow(ctx, `SELECT backoff_until FROM gh_installation_budget WHERE installation_id=$1`, e.inst).Scan(&backoff)
	if p1 != "act" || e.notified.Load() != 0 || backoff == nil || !backoff.After(e.clock.Now().Add(9*time.Minute)) {
		t.Fatalf("p1_mode=%s notified=%d backoff=%v", p1, e.notified.Load(), backoff)
	}
	if got := e.get(t, a.ID); got.State != StateAmbiguous {
		t.Fatalf("row: %+v", got)
	}
	e.wantPosts(t, 1)
}

func TestRerunBudgetExhaustedBeforePostStaysScheduledThenRunsOnce(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = accepted
	e.remaining.Store(1000) // below the 1,500 floor
	resetAt := e.clock.Now().Add(30 * time.Minute)
	e.reset.Store(resetAt.Unix())
	a := e.schedule(t, 1008)
	e.runOne(t)
	got := e.get(t, a.ID)
	if got.State != StateScheduled || got.PostStartedAt != nil || got.DueAt.Before(resetAt) {
		t.Fatalf("deferred row: %+v", got)
	}
	e.wantPosts(t, 0)

	e.clock.Advance(31 * time.Minute)
	e.remaining.Store(5000)
	e.reset.Store(e.clock.Now().Add(time.Hour).Unix())
	e.runOne(t)
	if got := e.get(t, a.ID); got.State != StateDone {
		t.Fatalf("after reset: %+v", got)
	}
	e.settleAll(t)
	e.wantPosts(t, 1)
}

func TestRerunRunReadFailureSendsNothingAndRetriesBounded(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = accepted
	e.gh.getErr = errors.New("dial tcp: i/o timeout")
	a := e.schedule(t, 1009)
	for i := 0; i < MaxAttempts+2; i++ {
		e.clock.Advance(10 * time.Minute)
		e.runOne(t)
	}
	if got := e.get(t, a.ID); got.State != StateFailed || got.Reason != ReasonAttemptsExhausted {
		t.Fatalf("row: %+v", got)
	}
	e.wantPosts(t, 0)
}

func TestRerunConcurrentWorkersPostOnce(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = accepted
	e.schedule(t, 1010)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = e.ledger.RunOne(context.Background()) }()
	}
	wg.Wait()
	e.wantPosts(t, 1)
}

func TestDueForRerun(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	reset := now.Add(20 * time.Minute)
	for _, c := range []struct {
		name  string
		class Class
		reset time.Time
		want  time.Time
	}{
		{"transient waits two minutes", ClassTransient, time.Time{}, now.Add(2 * time.Minute)},
		{"rate limit waits for the reset", ClassRateLimit, reset, reset},
		{"rate limit with a past reset waits two minutes", ClassRateLimit, now.Add(-time.Minute), now.Add(2 * time.Minute)},
		{"rate limit with an unknown reset waits two minutes", ClassRateLimit, time.Time{}, now.Add(2 * time.Minute)},
	} {
		if got := DueForRerun(c.class, now, c.reset); !got.Equal(c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestEscalationAfterRerun(t *testing.T) {
	if p, ok := EscalationAfterRerun(ClassRateLimit); !ok || p != "P4" {
		t.Errorf("rate limit: %q %v", p, ok)
	}
	if p, ok := EscalationAfterRerun(ClassTransient); !ok || p != "P2" {
		t.Errorf("transient: %q %v", p, ok)
	}
	if _, ok := EscalationAfterRerun(ClassCode); ok {
		t.Error("code_or_test has no P1 to escalate from")
	}
}

func TestScheduleP1SecondFailureEscalatesInsteadOfSecondRerun(t *testing.T) {
	e := newRerunEnv(t)
	ctx := context.Background()
	run := Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: 1011, RunAttempt: 1}
	r1, err := ScheduleP1(ctx, e.ledger, run, ClassRateLimit, "sig", e.clock.Now().Add(10*time.Minute), e.clock.Now())
	if err != nil || r1.Protocol != "P1" || r1.Action.State != StateScheduled {
		t.Fatalf("first: %+v %v", r1, err)
	}
	run.RunAttempt = 2 // the rerun failed too: same original run id
	r2, err := ScheduleP1(ctx, e.ledger, run, ClassRateLimit, "sig", time.Time{}, e.clock.Now())
	if err != nil || r2.Protocol != "P4" || r2.Action.ID != r1.Action.ID {
		t.Fatalf("second rate-limit failure: %+v %v", r2, err)
	}
	r3, err := ScheduleP1(ctx, e.ledger, run, ClassTransient, "sig", time.Time{}, e.clock.Now())
	if err != nil || r3.Protocol != "P2" {
		t.Fatalf("second transient failure: %+v %v", r3, err)
	}
}

func TestScheduleP1RefusesStartupFailureWithNoJobsClass(t *testing.T) {
	e := newRerunEnv(t)
	run := Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: 1012, RunAttempt: 1}
	if _, err := ScheduleP1(context.Background(), e.ledger, run, ClassConfig, "", time.Time{}, e.clock.Now()); err == nil {
		t.Fatal("config_or_secret must never be scheduled for a rerun")
	}
}

func TestScheduleP1RepeatFailureWithNoRerunOfOursEscalatesWithoutScheduling(t *testing.T) {
	e := newRerunEnv(t)
	run := Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: 1013, RunAttempt: 2}
	r, err := ScheduleP1(context.Background(), e.ledger, run, ClassTransient, "", time.Time{}, e.clock.Now())
	if err != nil || r.Protocol != "P2" || r.Action.ID != "" || e.count(t, "x") != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_watch_action WHERE workspace_id=$1::uuid`, e.ws).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows scheduled", n)
	}
}
