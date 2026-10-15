package service

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-1727 review (round 3): the pause decision must be serialized with the
// enqueue of a wakeup run. The tests freeze a dispatch at a known point with a
// database lock, never with a sleep, and wait on observed lock state.

// flipPause is a pause or resume writer: it takes the workspace row lock, the
// way the workspace update does, and a resume also settles what the pause held.
func flipPause(t *testing.T, f principalFixture, s *IssueWakeupService, paused bool) error {
	ctx := context.Background()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workspace WHERE id=$1 FOR UPDATE`, f.WorkspaceID); err != nil {
		return err
	}
	if paused {
		_, err = tx.Exec(ctx, `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)||'{"wakeups_paused":true}'::jsonb WHERE id=$1`, f.WorkspaceID)
	} else {
		if _, err = tx.Exec(ctx, `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)-'wakeups_paused' WHERE id=$1`, f.WorkspaceID); err == nil {
			err = s.SettleWakeupsOnResume(ctx, tx, parseTestUUID(t, f.WorkspaceID))
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// gateStarter parks the nth transaction start of a service until the test lets
// it go. A dispatch that read the pause flag before its enqueue transaction
// stops here with a stale answer; one that reads the flag inside the
// transaction has not read it yet.
type gateStarter struct {
	inner   TxStarter
	nth     int
	reached chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (g *gateStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	g.mu.Lock()
	g.calls++
	gate := g.calls == g.nth
	g.mu.Unlock()
	if gate {
		close(g.reached)
		<-g.release
	}
	return g.inner.Begin(ctx)
}

// pauseBetweenReadAndEnqueue commits a pause while a dispatch is parked before
// its enqueue transaction, then lets the dispatch go. Nothing may be enqueued.
func pauseBetweenReadAndEnqueue(t *testing.T, f principalFixture, s *IssueWakeupService, nth int, dispatch func() error, runs func() int) {
	gate := &gateStarter{inner: s.Tasks.TxStarter, nth: nth, reached: make(chan struct{}), release: make(chan struct{})}
	original := s.Tasks.TxStarter
	s.Tasks.TxStarter = gate
	t.Cleanup(func() { s.Tasks.TxStarter = original })
	dispatchErr := make(chan error, 1)
	go func() { dispatchErr <- dispatch() }()
	select {
	case <-gate.reached:
	case err := <-dispatchErr:
		t.Fatalf("dispatch ended before its transaction %d: %v", nth, err)
	}
	// The pause writer takes the workspace lock the dispatch has not taken yet.
	if err := flipPause(t, f, s, true); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-dispatchErr; err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if n := runs(); n != 0 {
		t.Fatalf("%d runs were enqueued after the pause committed", n)
	}
}

func TestPauseCommittingBeforeEnqueueCannotBeOutrunByDispatch(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "look"})
	f.Comment(t, util.UUIDToString(issue), "input before the pause")
	pauseBetweenReadAndEnqueue(t, f, s, 1, func() error { return s.dispatch(context.Background(), w) }, func() int { return wakeRuns(t, f, w.ID) })
}

func TestPauseCommittingBeforeEnqueueCannotBeOutrunBySystemDispatch(t *testing.T) {
	e := newBuiltinEnv(t)
	// The first transaction records the event; the second is the enqueue.
	pauseBetweenReadAndEnqueue(t, e.f, e.s, 2, func() error {
		return e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget",
			Number: 77, URL: "https://github.com/acme/widget/pull/77", MergeCommit: "merge-77", BaseBranch: "main", HeadBranch: "feature"})
	}, func() int { return e.tasks(t, SystemRulePRMerged) })
}

func TestResumeBeforeAStalePausedReadDoesNotDropLaterInputs(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "look"})
	if err := flipPause(t, f, s, true); err != nil {
		t.Fatal(err)
	}
	gate := &gateStarter{inner: s.Tasks.TxStarter, nth: 1, reached: make(chan struct{}), release: make(chan struct{})}
	original := s.Tasks.TxStarter
	s.Tasks.TxStarter = gate
	t.Cleanup(func() { s.Tasks.TxStarter = original })
	dispatchErr := make(chan error, 1)
	go func() { dispatchErr <- s.dispatch(context.Background(), w) }()
	select {
	case <-gate.reached:
	case err := <-dispatchErr:
		t.Fatalf("dispatch ended before its first transaction: %v", err)
	}
	// A dispatch that read "paused" earlier is parked. The admin resumes, then an
	// input arrives.
	if err := flipPause(t, f, s, false); err != nil {
		t.Fatal(err)
	}
	f.Comment(t, util.UUIDToString(issue), "input after the resume")
	close(gate.release)
	if err := <-dispatchErr; err != nil {
		t.Fatal(err)
	}
	s.Tasks.TxStarter = original
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("an input that arrived after the resume started %d runs, want 1", n)
	}
}
