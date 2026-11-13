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
	pending bool // GET reports the run as not completed
}

func (f *fakeRerunGH) GetRun(context.Context, int64, string, string, int64) (RunState, http.Header, error) {
	f.gets.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return RunState{}, nil, f.getErr
	}
	return RunState{Attempt: f.attempt, Completed: !f.pending}, http.Header{}, nil
}

func (f *fakeRerunGH) RerunFailedJobs(context.Context, int64, string, string, int64) (RerunResponse, error) {
	f.posts.Add(1)
	return f.post()
}

func (f *fakeRerunGH) setAttempt(n int) { f.mu.Lock(); f.attempt = n; f.mu.Unlock() }

type rerunEnv struct {
	*actEnv
	gh        *fakeRerunGH
	rr        *Rerunner
	remaining atomic.Int32
	reset     atomic.Int64 // unix seconds
	inst      int64
}

func newRerunEnv(t *testing.T) *rerunEnv {
	t.Helper()
	e := &rerunEnv{actEnv: newLedgerEnv(t), gh: &fakeRerunGH{attempt: 1}, inst: rand.Int64N(1 << 40)}
	e.remaining.Store(4000)
	e.reset.Store(e.clock.Now().Add(time.Hour).Unix())
	budget := ghbudget.New(e.pool, ghbudget.FetcherFunc(func(context.Context, int64) (int, time.Time, error) {
		return int(e.remaining.Load()), time.Unix(e.reset.Load(), 0), nil
	}), ghbudget.WithClock(e.clock.Now), ghbudget.WithJitter(func() time.Duration { return 0 }))
	e.rr = &Rerunner{API: e.gh, Budget: budget, Now: e.clock.Now,
		Installation: func(context.Context, Action) (int64, error) { return e.inst, nil }}
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

// followup runs the recovery step a worker has for classified POST failures.
func (e *rerunEnv) followup(t *testing.T) {
	t.Helper()
	if _, err := e.ledger.RerunFollowup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// escalations counts the P2/P4 actions scheduled for one original run.
func (e *rerunEnv) escalations(t *testing.T, runID int64, kind string) int {
	t.Helper()
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_watch_action WHERE workspace_id=$1::uuid
		AND original_run_id=$2 AND action_kind=$3`, e.ws, runID, kind).Scan(&n)
	return n
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

func (e *rerunEnv) enableP1(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name, mode, p1_mode)
		VALUES ($1::uuid,'o',$2,'act','act')`, e.ws, e.repo); err != nil {
		t.Fatal(err)
	}
}

func (e *rerunEnv) p1Audits(t *testing.T) (mode string, audits int) {
	t.Helper()
	ctx := context.Background()
	_ = e.pool.QueryRow(ctx, `SELECT p1_mode FROM ci_watch_config WHERE workspace_id=$1::uuid`, e.ws).Scan(&mode)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM ci_watch_config_audit WHERE workspace_id=$1::uuid AND field='p1_mode'
		AND old_value='act' AND new_value='shadow'`, e.ws).Scan(&audits)
	return mode, audits
}

func permission403() (RerunResponse, error) {
	return RerunResponse{Status: 403, Message: "Resource not accessible by integration"}, nil
}

func TestRerunPermission403IsStoredThenFollowupSwitchesShadowAndNotifiesOnce(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = permission403
	a := e.schedule(t, 1006)
	e.runOne(t)
	if got := e.get(t, a.ID); got.State != StateAmbiguous || got.PostOutcome != OutcomePermission {
		t.Fatalf("after 403: %+v", got)
	}
	e.followup(t)
	e.followup(t) // a second pass finds nothing left to do
	mode, audits := e.p1Audits(t)
	if mode != "shadow" || audits != 1 || e.escalations(t, 1006, ActionInboxNotify) != 1 {
		t.Fatalf("p1_mode=%s audits=%d notices=%d", mode, audits, e.escalations(t, 1006, ActionInboxNotify))
	}
	e.settleAll(t)
	e.followup(t)
	if _, audits := e.p1Audits(t); audits != 1 || e.escalations(t, 1006, ActionInboxNotify) != 1 {
		t.Fatalf("effects repeated: audits=%d notices=%d", audits, e.escalations(t, 1006, ActionInboxNotify))
	}
	e.wantPosts(t, 1)
}

func TestRerunPermission403FollowupFailureLosesNothingAndRetries(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = permission403
	e.schedule(t, 1014)
	e.runOne(t)
	dead, cancel := context.WithCancel(context.Background())
	cancel() // the database call fails at the first boundary
	if _, err := e.ledger.RerunFollowup(dead); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
	if mode, audits := e.p1Audits(t); mode != "act" || audits != 0 || e.escalations(t, 1014, ActionInboxNotify) != 0 {
		t.Fatalf("half applied: p1_mode=%s audits=%d", mode, audits)
	}
	e.followup(t) // the next pass, after a restart, does both
	if mode, audits := e.p1Audits(t); mode != "shadow" || audits != 1 || e.escalations(t, 1014, ActionInboxNotify) != 1 {
		t.Fatalf("p1_mode=%s audits=%d notices=%d", mode, audits, e.escalations(t, 1014, ActionInboxNotify))
	}
	e.wantPosts(t, 1)
}

func TestRerunFollowupConcurrentWorkersApplyEffectsOnce(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = permission403
	e.schedule(t, 1021)
	e.runOne(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.ledger.RerunFollowup(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if mode, audits := e.p1Audits(t); mode != "shadow" || audits != 1 || e.escalations(t, 1021, ActionInboxNotify) != 1 {
		t.Fatalf("p1_mode=%s audits=%d notices=%d", mode, audits, e.escalations(t, 1021, ActionInboxNotify))
	}
	e.wantPosts(t, 1)
}

func TestRerunRateLimit403BacksOffThenFollowupEscalatesToP4WithoutShadow(t *testing.T) {
	e := newRerunEnv(t)
	ctx := context.Background()
	e.enableP1(t)
	e.gh.post = func() (RerunResponse, error) {
		h := http.Header{"Retry-After": []string{"600"}}
		return RerunResponse{Status: 403, Header: h, Message: "You have exceeded a secondary rate limit"}, nil
	}
	a := e.schedule(t, 1007)
	e.runOne(t)
	var backoff *time.Time
	_ = e.pool.QueryRow(ctx, `SELECT backoff_until FROM gh_installation_budget WHERE installation_id=$1`, e.inst).Scan(&backoff)
	if backoff == nil || !backoff.After(e.clock.Now().Add(9*time.Minute)) {
		t.Fatalf("backoff=%v", backoff)
	}
	if got := e.get(t, a.ID); got.State != StateAmbiguous || got.PostOutcome != OutcomeRateLimit {
		t.Fatalf("row: %+v", got)
	}
	e.followup(t)
	e.followup(t)
	if mode, _ := e.p1Audits(t); mode != "act" || e.escalations(t, 1007, ActionInboxNotify) != 1 {
		t.Fatalf("p1_mode=%s notices=%d", mode, e.escalations(t, 1007, ActionInboxNotify))
	}
	e.wantPosts(t, 1)
}

func Test5xxPostEscalatesToP2OnlyAfterReconcileFindsNothing(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = func() (RerunResponse, error) { return RerunResponse{Status: 502, Message: "bad gateway"}, nil }
	a := e.schedule(t, 1015)
	e.runOne(t)
	e.followup(t) // may have landed: nothing yet
	if e.escalations(t, 1015, ActionIssueCreate) != 0 {
		t.Fatal("escalated before reconcile")
	}
	e.settleAll(t)
	if got := e.get(t, a.ID); got.State != StateFailed || got.PostOutcome != OutcomeServer {
		t.Fatalf("row: %+v", got)
	}
	e.followup(t)
	e.followup(t)
	if e.escalations(t, 1015, ActionIssueCreate) != 1 || e.escalations(t, 1015, ActionInboxNotify) != 0 {
		t.Fatalf("P2 issues=%d", e.escalations(t, 1015, ActionIssueCreate))
	}
	e.wantPosts(t, 1)
}

func Test5xxPostThatLandedNeverEscalates(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = func() (RerunResponse, error) { return RerunResponse{Status: 500}, nil }
	e.schedule(t, 1016)
	e.runOne(t)
	e.gh.setAttempt(2)
	e.settleAll(t)
	e.followup(t)
	if e.escalations(t, 1016, ActionIssueCreate)+e.escalations(t, 1016, ActionInboxNotify) != 0 {
		t.Fatal("escalated a rerun that landed")
	}
	e.wantPosts(t, 1)
}

func TestTimeoutPostEscalatesToP2WhenNotLanded(t *testing.T) {
	e := newRerunEnv(t)
	e.gh.post = func() (RerunResponse, error) { return RerunResponse{}, context.DeadlineExceeded }
	a := e.schedule(t, 1017)
	e.runOne(t)
	if got := e.get(t, a.ID); got.PostOutcome != OutcomeNoAnswer {
		t.Fatalf("outcome %q", got.PostOutcome)
	}
	e.settleAll(t)
	e.followup(t)
	e.followup(t)
	if e.escalations(t, 1017, ActionIssueCreate) != 1 {
		t.Fatalf("P2 issues=%d", e.escalations(t, 1017, ActionIssueCreate))
	}
	if mode, _ := e.p1Audits(t); mode == "shadow" {
		t.Fatal("a timeout switched the repo to shadow")
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

func TestScheduleP1DuplicateDeliveryOfAttempt1DoesNothing(t *testing.T) {
	e := newRerunEnv(t)
	ctx := context.Background()
	run := Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: 1011, RunAttempt: 1}
	r1, err := ScheduleP1(ctx, e.ledger, run, ClassRateLimit, "sig", e.clock.Now().Add(10*time.Minute), e.clock.Now())
	if err != nil || r1.Protocol != "P1" || r1.Action.State != StateScheduled {
		t.Fatalf("first: %+v %v", r1, err)
	}
	var wg sync.WaitGroup
	routes := make(chan Route, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := ScheduleP1(ctx, e.ledger, run, ClassRateLimit, "sig", time.Time{}, e.clock.Now())
			if err != nil {
				t.Error(err)
			}
			routes <- r
		}()
	}
	wg.Wait()
	close(routes)
	for r := range routes {
		if r.Protocol != "" {
			t.Fatalf("redelivery of attempt 1 routed to %q", r.Protocol)
		}
	}
	if e.escalations(t, 1011, ActionInboxNotify)+e.escalations(t, 1011, ActionIssueCreate) != 0 {
		t.Fatal("a scheduled rerun was escalated")
	}
}

func TestScheduleP1SecondFailureEscalatesOnceWithAnAction(t *testing.T) {
	e := newRerunEnv(t)
	ctx := context.Background()
	run := Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: 1019, RunAttempt: 1}
	if _, err := ScheduleP1(ctx, e.ledger, run, ClassRateLimit, "sig", time.Time{}, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	run.RunAttempt = 2 // the rerun failed too: same original run id
	r2, err := ScheduleP1(ctx, e.ledger, run, ClassRateLimit, "sig", time.Time{}, e.clock.Now())
	if err != nil || r2.Protocol != "P4" || r2.Action.Kind != ActionInboxNotify || r2.Action.OriginalRunID != 1019 {
		t.Fatalf("second rate-limit failure: %+v %v", r2, err)
	}
	for i := 0; i < 2; i++ { // redelivered attempt 2, either class
		for _, c := range []Class{ClassRateLimit, ClassTransient} {
			if r, err := ScheduleP1(ctx, e.ledger, run, c, "sig", time.Time{}, e.clock.Now()); err != nil || r.Protocol != "" {
				t.Fatalf("redelivery: %+v %v", r, err)
			}
		}
	}
	if e.escalations(t, 1019, ActionInboxNotify) != 1 || e.escalations(t, 1019, ActionIssueCreate) != 0 {
		t.Fatal("want exactly one escalation")
	}
}

func TestScheduleP1SecondTransientFailureEscalatesToP2Issue(t *testing.T) {
	e := newRerunEnv(t)
	run := Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: 1020, RunAttempt: 2}
	r, err := ScheduleP1(context.Background(), e.ledger, run, ClassTransient, "sig", time.Time{}, e.clock.Now())
	if err != nil || r.Protocol != "P2" || r.Action.Kind != ActionIssueCreate || r.Action.SignatureHash != "sig" {
		t.Fatalf("%+v %v", r, err)
	}
	if e.escalations(t, 1020, ActionRerun) != 0 {
		t.Fatal("a repeat failure scheduled a second rerun")
	}
}

func TestScheduleP1RefusesStartupFailureWithNoJobsClass(t *testing.T) {
	e := newRerunEnv(t)
	run := Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: 1012, RunAttempt: 1}
	if _, err := ScheduleP1(context.Background(), e.ledger, run, ClassConfig, "", time.Time{}, e.clock.Now()); err == nil {
		t.Fatal("config_or_secret must never be scheduled for a rerun")
	}
}
