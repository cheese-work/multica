package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

// gatedInsertTx parks the first receipt insert of a transaction until the test
// lets it go, so a pull request event can be frozen between its read of the
// workspace and its commit.
type gatedInsertTx struct {
	pgx.Tx
	once    *sync.Once
	reached chan struct{}
	release chan struct{}
}

func (g gatedInsertTx) gate(sql string) {
	if strings.Contains(sql, "INSERT INTO issue_wakeup_receipt") {
		g.once.Do(func() {
			close(g.reached)
			<-g.release
		})
	}
}

func (g gatedInsertTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	g.gate(sql)
	return g.Tx.QueryRow(ctx, sql, args...)
}

func (g gatedInsertTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	g.gate(sql)
	return g.Tx.Exec(ctx, sql, args...)
}

type gatedInsertStarter struct {
	inner TxStarter
	once  *sync.Once
	reach chan struct{}
	free  chan struct{}
}

func (g gatedInsertStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := g.inner.Begin(ctx)
	return gatedInsertTx{Tx: tx, once: g.once, reached: g.reach, release: g.free}, err
}

// A pull request event whose receipt commits after the admin's resume swept the
// receipts must not fire: the event arrived while the workspace was paused.
func TestPullRequestEventCommittingAfterResumeSweepDoesNotFire(t *testing.T) {
	e := newBuiltinEnv(t)
	if err := flipPause(t, e.f, e.s, true); err != nil {
		t.Fatal(err)
	}
	starter := gatedInsertStarter{inner: e.s.Tasks.TxStarter, once: &sync.Once{}, reach: make(chan struct{}), free: make(chan struct{})}
	original := e.s.Tasks.TxStarter
	e.s.Tasks.TxStarter = starter
	t.Cleanup(func() { e.s.Tasks.TxStarter = original })
	triggerErr := make(chan error, 1)
	go func() {
		triggerErr <- e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget",
			Number: 88, URL: "https://github.com/acme/widget/pull/88", MergeCommit: "merge-88", BaseBranch: "main", HeadBranch: "feature"})
	}()
	select {
	case <-starter.reach:
	case err := <-triggerErr:
		t.Fatalf("trigger ended before it recorded its receipt: %v", err)
	}
	// The admin resumes while the event is in flight. The resume either finishes
	// first (the bug) or waits for the event's transaction (the fix).
	resumeErr := make(chan error, 1)
	go func() { resumeErr <- flipPause(t, e.f, e.s, false) }()
	resumeSettled := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !resumeSettled; {
		select {
		case err := <-resumeErr:
			if err != nil {
				t.Fatal(err)
			}
			resumeSettled, resumeErr = true, nil
		default:
			// Observed state, not a delay: the resume is parked on a lock.
			if e.f.Count(t, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query ILIKE '%FROM workspace WHERE id=%FOR UPDATE%'`) > 0 {
				resumeSettled = true
			}
		}
	}
	if !resumeSettled {
		t.Fatal("the resume neither finished nor waited on a lock")
	}
	close(starter.free)
	if err := <-triggerErr; err != nil {
		t.Fatal(err)
	}
	if resumeErr != nil {
		if err := <-resumeErr; err != nil {
			t.Fatal(err)
		}
	}
	e.s.Tasks.TxStarter = original
	if err := e.s.TickWorkspaces(context.Background(), parseTestUUID(t, e.f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if n := e.tasks(t, SystemRulePRMerged); n != 0 {
		t.Fatalf("an event that arrived while paused started %d runs after the resume", n)
	}
}

// CHE-1727 review (round 5): every receipt producer must honor the no-replay
// rule without a lock the producer cannot take safely, and sub-issue hints must
// keep prompting an evaluation of what still holds.

func childDoneRule(t *testing.T, f principalFixture, issue pgtype.UUID) db.IssueWakeup {
	t.Helper()
	w, err := f.q.GetSystemWakeup(context.Background(), db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRuleChildDone)})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// The last sub-issue closing while paused is a state that still holds on
// resume. Its hint only prompts an evaluation, so the pause must not drop it.
func TestChildDoneHintRecordedWhilePausedEvaluatesOnResume(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	f.Exec(t, "UPDATE issue SET status='in_progress',assignee_type='agent',assignee_id=$2 WHERE id=$1", issue, agent)
	child := f.Issue(t, "child", testutil.Cols{"parent_issue_id": issue, "status": "in_progress"})
	f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", issue)
	if err := s.ProcessChildEvents(ctx, issue); err != nil {
		t.Fatal(err)
	}
	if err := flipPause(t, f, s, true); err != nil {
		t.Fatal(err)
	}
	f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", child)
	if err := s.ProcessChildEvents(ctx, issue); err != nil {
		t.Fatal(err)
	}
	rule := childDoneRule(t, f, issue)
	if n := wakeRuns(t, f, rule.ID); n != 0 {
		t.Fatalf("paused workspace started %d runs for a sub-issue closing", n)
	}
	if err := flipPause(t, f, s, false); err != nil {
		t.Fatal(err)
	}
	tickWorkspace(t, f, s)
	if n := wakeRuns(t, f, rule.ID); n != 1 {
		t.Fatalf("a closing that still holds started %d runs after the resume, want 1", n)
	}
}

// A receipt a producer inserted while the workspace was paused, but committed
// after the resume swept, is still a paused-era input. The producer here is an
// issue update: unlike a comment insert, whose foreign key to the workspace
// already makes a resume wait for it, an issue update takes no workspace lock.
func pausedIssueUpdateCommittingAfterResume(t *testing.T, f principalFixture, s *IssueWakeupService, issue pgtype.UUID, afterResume func()) {
	t.Helper()
	ctx := context.Background()
	if err := flipPause(t, f, s, true); err != nil {
		t.Fatal(err)
	}
	producer, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Rollback(ctx)
	if _, err := producer.Exec(ctx, "UPDATE issue SET status='in_progress' WHERE id=$1", issue); err != nil {
		t.Fatal(err)
	}
	// The resume runs while the producer's transaction is still open, so its
	// sweep cannot see the receipt the update's trigger wrote.
	if err := flipPause(t, f, s, false); err != nil {
		t.Fatal(err)
	}
	if err := producer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	afterResume()
}

func TestEventInsertedWhilePausedAndCommittedAfterResumeIsDropped(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"issue.status_changed"}, Instruction: "look"})
	pausedIssueUpdateCommittingAfterResume(t, f, s, issue, func() {})
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("an event produced while paused started %d runs after the resume", n)
	}
}

// A later event merges into the late paused-era receipt. It is a real post-resume
// event, so exactly one run starts for it.
func TestEventAfterResumeStillFiresWhenItMergesIntoALatePausedReceipt(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"issue.status_changed"}, Instruction: "look"})
	pausedIssueUpdateCommittingAfterResume(t, f, s, issue, func() {
		f.Exec(t, "UPDATE issue SET status='in_review' WHERE id=$1", issue)
	})
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("an event after the resume started %d runs, want 1", n)
	}
}
