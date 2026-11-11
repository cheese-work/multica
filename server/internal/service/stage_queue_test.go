package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/stagequeue"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type stageFixture struct {
	*testutil.Fixture
	q       *db.Queries
	agentID string
	runtime string
	prSeq   int
}

func newStageFixture(t *testing.T, limits stagequeue.Limits) *stageFixture {
	t.Helper()
	prev := stageLimits
	stageLimits = limits
	t.Cleanup(func() { stageLimits = prev })
	pool := sharedTestPool(t)
	fx := testutil.New(pool, "", "")
	suffix := time.Now().UnixNano()
	fx.UserID = fx.User(t, "Stage queue", fmt.Sprintf("stage-queue-%d@multica.test", suffix))
	fx.WorkspaceID = fx.Workspace(t, "Stage queue", fmt.Sprintf("stage-queue-%d", suffix))
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	rt := fx.Runtime(t, "Stage queue runtime")
	return &stageFixture{Fixture: fx, q: db.New(pool), agentID: fx.Agent(t, "Lane owner", rt), runtime: rt}
}

// reviewIssue is an agent_review issue assigned to the owner with a linked open
// PR created `age` ago.
func (f *stageFixture) reviewIssue(t *testing.T, title string, age time.Duration) string {
	t.Helper()
	id := f.Issue(t, title, testutil.Cols{"status": "agent_review", "assignee_type": "agent", "assignee_id": f.agentID})
	f.link(t, id, age)
	return id
}

func (f *stageFixture) link(t *testing.T, issueID string, age time.Duration) {
	t.Helper()
	f.prSeq++
	pr := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
		"pr_number": f.prSeq + int(time.Now().UnixNano()%1000000), "title": "t", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": time.Now().Add(-age), "pr_updated_at": time.Now(),
	})
	f.InsertNoID(t, "issue_pull_request", testutil.Cols{"issue_id": issueID, "pull_request_id": pr},
		"issue_id = $1 AND pull_request_id = $2", issueID, pr)
}

// gateIssue is "<stage> gate: <parent>" assigned to the lane owner, under a
// parent work item whose PR was created `age` ago.
func (f *stageFixture) gateIssue(t *testing.T, stage, parent string) string {
	t.Helper()
	return f.Issue(t, stage+" gate: "+parent, testutil.Cols{
		"parent_issue_id": parent, "assignee_type": "agent", "assignee_id": f.agentID,
	})
}

func (f *stageFixture) run(t *testing.T, issueID, status string) string {
	t.Helper()
	return f.Task(t, f.agentID, testutil.Cols{"issue_id": issueID, "status": status, "runtime_id": f.runtime})
}

func (f *stageFixture) guard(issueID string) error {
	_, err := guardIssueRunGate(context.Background(), f.q, util.MustParseUUID(issueID), OriginDerived,
		RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: "assignment or status handoff", ReviewRun: true})
	return err
}

// The (N+1)th PR for the review stage is refused; its slot frees when the
// active run ends.
func TestStageQueueReviewRefusesNPlusOne(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	a := f.reviewIssue(t, "PR A", 3*time.Hour)
	b := f.reviewIssue(t, "PR B", 1*time.Hour)
	runA := f.run(t, a, "running")

	if err := f.guard(b); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("B while A runs: err = %v, want ErrStageQueueFull", err)
	}
	if err := f.guard(a); err != nil {
		t.Fatalf("A (holds the slot) was refused: %v", err)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, runA)
	if err := f.guard(b); err != nil {
		t.Fatalf("B after A finished: %v", err)
	}
}

// A refused PR is started by the release job once a slot frees, exactly once,
// and a PR whose review already ran is not started again.
func TestReleaseStageQueuesStartsRefusedPROnce(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	a := f.reviewIssue(t, "PR A", 3*time.Hour)
	b := f.reviewIssue(t, "PR B", 1*time.Hour)
	runA := f.run(t, a, "running")
	if err := f.guard(b); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("B while A runs: err = %v, want ErrStageQueueFull", err)
	}
	var released []string
	enqueue := func(_ context.Context, issue db.Issue, _, _ pgtype.UUID) error {
		released = append(released, util.UUIDToString(issue.ID))
		// A real enqueue inserts the task; mimic it so B now holds the slot.
		f.run(t, util.UUIDToString(issue.ID), "queued")
		return nil
	}
	if err := releaseStageQueues(context.Background(), f.q, enqueue); err != nil || len(released) != 0 {
		t.Fatalf("release while A runs: released %v, err %v; want none", released, err)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, runA)
	if err := releaseStageQueues(context.Background(), f.q, enqueue); err != nil || len(released) != 1 || released[0] != b {
		t.Fatalf("release after A finished: released %v, err %v; want only B %s", released, err, b)
	}
	// B's run ends; A and B stay in review but neither is refused, so no loop.
	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE issue_id = $1`, b)
	if err := releaseStageQueues(context.Background(), f.q, enqueue); err != nil || len(released) != 1 {
		t.Fatalf("release after B finished: released %v, err %v; want no new run", released, err)
	}
}

// With N = 2 two reviews run and the third waits. A reviewer's second PR does
// not count against another reviewer's slots.
func TestStageQueueCapacityIsPerOwner(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 2})
	for i := 0; i < 2; i++ {
		f.run(t, f.reviewIssue(t, fmt.Sprintf("running %d", i), time.Hour), "dispatched")
	}
	third := f.reviewIssue(t, "third", time.Hour)
	if err := f.guard(third); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("third: err = %v, want ErrStageQueueFull", err)
	}
}

// A person naming the agent by hand, and a run that is not a review run, are
// not queued.
func TestStageQueueSkipsNamedRunsAndNonReviewTargets(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	f.run(t, f.reviewIssue(t, "A", time.Hour), "running")
	b := f.reviewIssue(t, "B", time.Hour)
	ctx := context.Background()
	target := RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: "mention", ReviewRun: true}
	if _, err := guardIssueRunGate(ctx, f.q, util.MustParseUUID(b), OriginNamed, target); err != nil {
		t.Fatalf("named run refused: %v", err)
	}
	target.ReviewRun = false
	if _, err := guardIssueRunGate(ctx, f.q, util.MustParseUUID(b), OriginDerived, target); err != nil {
		t.Fatalf("non-review run refused: %v", err)
	}
}

// Device lane, N = 1, stacked PRs: predecessor and successor each own a device
// gate. The waiting one holds no slot, so both progress in turn.
func TestStageQueueDeviceLaneStackedPRsBothProgress(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageDevice: 1})
	pred := f.Issue(t, "predecessor", testutil.Cols{"status": "in_progress"})
	succ := f.Issue(t, "successor", testutil.Cols{"status": "in_progress", "parent_issue_id": pred})
	f.link(t, pred, 5*time.Hour)
	f.link(t, succ, 4*time.Hour)
	gPred, gSucc := f.gateIssue(t, "device", pred), f.gateIssue(t, "device", succ)

	runSucc := f.run(t, gSucc, "running")
	if err := f.guard(gPred); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("pred gate while succ runs: err = %v, want ErrStageQueueFull", err)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, runSucc)
	f.Exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, gSucc)
	if err := f.guard(gPred); err != nil {
		t.Fatalf("pred gate after succ finished: %v", err)
	}
}

// A browser gate does not use the device lane's slot, and a gate for another
// stage title is never queued.
func TestStageQueueStagesAreIndependent(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageDevice: 1, stagequeue.StageBrowser: 1})
	p1 := f.Issue(t, "p1", testutil.Cols{"status": "in_progress"})
	p2 := f.Issue(t, "p2", testutil.Cols{"status": "in_progress"})
	f.run(t, f.gateIssue(t, "device", p1), "running")
	if err := f.guard(f.gateIssue(t, "browser", p2)); err != nil {
		t.Fatalf("browser gate refused by a device run: %v", err)
	}
	sec := f.Issue(t, "security gate: x", testutil.Cols{"parent_issue_id": p2, "assignee_type": "agent", "assignee_id": f.agentID})
	if err := f.guard(sec); err != nil {
		t.Fatalf("security gate queued: %v", err)
	}
}

// Release starts the queued PRs closest to merge first, only as many as there
// are free slots, and never one that already runs.
func TestReleaseStageQueuesOrdersByOpenGatesThenAge(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 2})
	oldNoGate := f.reviewIssue(t, "old, no gates", 9*time.Hour)
	newNoGate := f.reviewIssue(t, "new, no gates", 2*time.Hour)
	gated := f.reviewIssue(t, "old, one open gate", 20*time.Hour)
	f.Issue(t, "security gate: "+gated, testutil.Cols{"parent_issue_id": gated, "status": "todo"})
	running := f.reviewIssue(t, "already running", 30*time.Hour)
	f.run(t, running, "running")

	for _, id := range []string{oldNoGate, newNoGate, gated} {
		f.Exec(t, `INSERT INTO stage_queue_waiting (issue_id, agent_id) VALUES ($1, $2)`, id, f.agentID)
	}
	var released []string
	err := releaseStageQueues(context.Background(), f.q, func(_ context.Context, issue db.Issue, _, _ pgtype.UUID) error {
		released = append(released, util.UUIDToString(issue.ID))
		return nil
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	// N = 2, one slot held by "already running": one free slot, to the oldest
	// of the PRs with no open gate.
	if len(released) != 1 || released[0] != oldNoGate {
		t.Fatalf("released %v, want only %s (old, no gates); new=%s gated=%s", released, oldNoGate, newNoGate, gated)
	}
}

// The snapshot shows a refused PR queued behind the one that runs, and shows
// nothing for work that is not stage work.
func TestLoadStageQueueSnapshotShowsAdmittedAndQueued(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	a := f.reviewIssue(t, "PR A", 3*time.Hour)
	b := f.reviewIssue(t, "PR B", 1*time.Hour)
	f.run(t, a, "running")
	if err := f.guard(b); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("B while A runs: err = %v, want ErrStageQueueFull", err)
	}
	load := func(id string) StageQueueSnapshot {
		issue, err := f.q.GetIssue(context.Background(), util.MustParseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		snap, err := LoadStageQueueSnapshot(context.Background(), f.q, issue)
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	if s := load(a); s.State != "admitted" || len(s.Queued) != 1 || s.Queued[0] != b {
		t.Fatalf("A snapshot = %+v, want admitted with B queued", s)
	}
	if s := load(b); s.State != "queued" || s.Position != 0 || s.Limit != 1 {
		t.Fatalf("B snapshot = %+v, want queued at 0, limit 1", s)
	}
	plain := f.Issue(t, "plain", testutil.Cols{"status": "in_progress", "assignee_type": "agent", "assignee_id": f.agentID})
	if s := load(plain); s.State != "none" {
		t.Fatalf("plain issue snapshot = %+v, want none", s)
	}
}

func (f *stageFixture) service(t *testing.T) *TaskService {
	t.Helper()
	return &TaskService{Queries: f.q, TxStarter: sharedTestPool(t), Bus: events.New()}
}

func (f *stageFixture) tasksOn(t *testing.T, issueID string) int {
	t.Helper()
	return f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID)
}

func (f *stageFixture) marks(t *testing.T, issueID string) int {
	t.Helper()
	return f.Count(t, `SELECT count(*) FROM stage_queue_waiting WHERE issue_id = $1`, issueID)
}

// Through the real enqueue: the refused PR leaves no task, but its refusal is
// on record after the transaction's rollback, and the release job starts it
// once, when the slot frees.
func TestStageQueueRealEnqueueRecordsRefusalAndReleaseStartsIt(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	svc := f.service(t)
	a := f.reviewIssue(t, "PR A", 3*time.Hour)
	b := f.reviewIssue(t, "PR B", 1*time.Hour)
	runA := f.run(t, a, "running")
	issueB, err := f.q.GetIssue(context.Background(), util.MustParseUUID(b))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.EnqueueTaskForIssue(context.Background(), issueB); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("enqueue B while A runs: err = %v, want ErrStageQueueFull", err)
	}
	if got := f.tasksOn(t, b); got != 0 {
		t.Fatalf("B has %d tasks after a refusal, want 0", got)
	}
	if got := f.marks(t, b); got != 1 {
		t.Fatalf("B has %d refusal rows, want 1", got)
	}

	if err := svc.ReleaseStageQueues(context.Background()); err != nil || f.tasksOn(t, b) != 0 {
		t.Fatalf("release while A runs: err %v, B tasks %d; want none", err, f.tasksOn(t, b))
	}
	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, runA)
	for i := 0; i < 2; i++ {
		if err := svc.ReleaseStageQueues(context.Background()); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	if got := f.tasksOn(t, b); got != 1 {
		t.Fatalf("B has %d tasks after release, want exactly 1", got)
	}
}

// A refusal that happens inside the enqueue transaction (the race the early
// check cannot see) rolls back with the transaction, yet stays on record.
func TestStageQueueRefusalInsideRolledBackTxStaysOnRecord(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	svc := f.service(t)
	f.run(t, f.reviewIssue(t, "PR A", 3*time.Hour), "running")
	b := f.reviewIssue(t, "PR B", 1*time.Hour)

	err := svc.runInTx(context.Background(), func(qtx *db.Queries) error {
		_, err := guardIssueRunGate(context.Background(), qtx, util.MustParseUUID(b), OriginDerived,
			RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: "assignment or status handoff", ReviewRun: true})
		return err
	})
	if !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("err = %v, want ErrStageQueueFull", err)
	}
	if got := f.marks(t, b); got != 1 {
		t.Fatalf("B has %d refusal rows after the rollback, want 1", got)
	}
}

// If the refusal cannot be written down, the caller gets an error, never a
// silent ErrStageQueueFull that nothing will ever retry.
func TestStageQueueRefusalWriteFailureIsReturned(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	svc := f.service(t)
	f.run(t, f.reviewIssue(t, "PR A", 3*time.Hour), "running")
	b := f.reviewIssue(t, "PR B", 1*time.Hour)
	issueB, err := f.q.GetIssue(context.Background(), util.MustParseUUID(b))
	if err != nil {
		t.Fatal(err)
	}
	f.Exec(t, `CREATE OR REPLACE FUNCTION stage_queue_test_fail() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'stage_queue_test_fail'; END $$ LANGUAGE plpgsql`)
	f.Exec(t, `CREATE TRIGGER stage_queue_test_fail BEFORE INSERT OR UPDATE ON stage_queue_waiting FOR EACH ROW EXECUTE FUNCTION stage_queue_test_fail()`)
	f.Cleanup(t, `DROP TRIGGER IF EXISTS stage_queue_test_fail ON stage_queue_waiting`)

	check := func(name string, err error) {
		t.Helper()
		if err == nil || errors.Is(err, ErrStageQueueFull) {
			t.Fatalf("%s: err = %v, want a record-refusal error that is not ErrStageQueueFull", name, err)
		}
	}
	_, err = svc.EnqueueTaskForIssue(context.Background(), issueB)
	check("enqueue", err)
	err = svc.runInTx(context.Background(), func(qtx *db.Queries) error {
		_, err := guardIssueRunGate(context.Background(), qtx, util.MustParseUUID(b), OriginDerived,
			RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: "assignment or status handoff", ReviewRun: true})
		return err
	})
	check("in-tx guard", err)
	if got := f.tasksOn(t, b); got != 0 {
		t.Fatalf("B has %d tasks, want 0", got)
	}
}

// releasedTask is the task a release created for the issue: who it runs as and
// what taint it carries.
func (f *stageFixture) releasedTask(t *testing.T, issueID string) (originator string, taint []string) {
	t.Helper()
	f.QueryRow(t, `SELECT COALESCE(originator_user_id::text, ''), taint_ids FROM agent_task_queue
		WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&originator, &taint)
	return originator, taint
}

// A refused PR is released as the person who asked for the run, not as the
// issue's creator: the creator's private agents stay out of reach (CWE-863).
func TestStageQueueReleaseKeepsTheRequestingPrincipal(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	svc := f.service(t)
	actor := f.User(t, "Requester", fmt.Sprintf("stage-actor-%d@multica.test", time.Now().UnixNano()))
	f.Member(t, f.WorkspaceID, actor, "member")
	runA := f.run(t, f.reviewIssue(t, "PR A", 3*time.Hour), "running")
	b := f.reviewIssue(t, "PR B", 1*time.Hour) // created by f.UserID
	issueB, err := f.q.GetIssue(context.Background(), util.MustParseUUID(b))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.EnqueueTaskForIssueByActor(context.Background(), issueB, util.MustParseUUID(actor)); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("enqueue B while A runs: err = %v, want ErrStageQueueFull", err)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, runA)
	if err := svc.ReleaseStageQueues(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got, _ := f.releasedTask(t, b); got != actor {
		t.Fatalf("released run originator = %q, want the requester %q (not the creator %q)", got, actor, f.UserID)
	}
}

// A refusal keeps the taint of the task that asked, and the released run
// carries it: release must not launder a tainted request into a clean run.
func TestStageQueueReleaseCarriesTheRequestersTaint(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	svc := f.service(t)
	runA := f.run(t, f.reviewIssue(t, "PR A", 3*time.Hour), "running")
	b := f.reviewIssue(t, "PR B", 1*time.Hour)
	issueB, err := f.q.GetIssue(context.Background(), util.MustParseUUID(b))
	if err != nil {
		t.Fatal(err)
	}
	other := f.Issue(t, "elsewhere", testutil.Cols{"status": "in_progress"})
	taintedTask := f.run(t, other, "running")
	f.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['ext-1'] WHERE id = $1`, taintedTask)
	ctx := WithActingTask(context.Background(), util.MustParseUUID(taintedTask))

	if _, err := svc.EnqueueTaskForIssue(ctx, issueB); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("enqueue B while A runs: err = %v, want ErrStageQueueFull", err)
	}
	// A second refusal from a differently tainted task adds to the record.
	f.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['ext-2'] WHERE id = $1`, taintedTask)
	if _, err := svc.EnqueueTaskForIssue(ctx, issueB); !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("second enqueue B: err = %v, want ErrStageQueueFull", err)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, runA)
	if err := svc.ReleaseStageQueues(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	_, taint := f.releasedTask(t, b)
	if len(taint) != 2 || taint[0] != "ext-1" || taint[1] != "ext-2" {
		t.Fatalf("released run taint = %v, want [ext-1 ext-2]", taint)
	}
}

// The comment-triggered enqueue opens its own transaction. Its stage refusal
// rolls back with it, so the refusal is written again afterwards, with the
// requester, the trigger comment and the taint (CHE-3611).
func TestStageQueueCommentTriggeredRefusalStaysOnRecordAndReleases(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	svc := f.service(t)
	actor := f.User(t, "Requester", fmt.Sprintf("stage-comment-actor-%d@multica.test", time.Now().UnixNano()))
	f.Member(t, f.WorkspaceID, actor, "member")
	runA := f.run(t, f.reviewIssue(t, "PR A", 3*time.Hour), "running")
	b := f.reviewIssue(t, "PR B", 1*time.Hour)
	issueB, err := f.q.GetIssue(context.Background(), util.MustParseUUID(b))
	if err != nil {
		t.Fatal(err)
	}
	comment := f.Comment(t, b, "please review", testutil.Cols{"author_id": actor})
	taintedTask := f.run(t, f.Issue(t, "elsewhere", testutil.Cols{"status": "in_progress"}), "running")
	f.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['ext-1'] WHERE id = $1`, taintedTask)
	ctx := WithActingTask(context.Background(), util.MustParseUUID(taintedTask))

	_, err = svc.EnqueueCommentTriggeredTask(ctx, issueB, util.MustParseUUID(f.agentID), util.MustParseUUID(comment),
		true, false, pgtype.UUID{}, "", util.MustParseUUID(actor), OriginDerived)
	if !errors.Is(err, ErrStageQueueFull) {
		t.Fatalf("comment-triggered enqueue of B while A runs: err = %v, want ErrStageQueueFull", err)
	}
	if got := f.tasksOn(t, b); got != 0 {
		t.Fatalf("B has %d tasks after a refusal, want 0", got)
	}
	if got := f.marks(t, b); got != 1 {
		t.Fatalf("B has %d refusal rows after the rollback, want 1", got)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, runA)
	if err := svc.ReleaseStageQueues(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	originator, taint := f.releasedTask(t, b)
	if originator != actor || len(taint) != 1 || taint[0] != "ext-1" {
		t.Fatalf("released run originator %q taint %v, want the requester %q with [ext-1]", originator, taint, actor)
	}
}

// CHE-3613: a full stage lists its waiting PRs, next first, and shows whether
// a queued-PR push task (a run of an agent other than the owner) is open.
func TestLoadFullStagesListsQueuedPRsAndOpenPushTask(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 1})
	a := f.reviewIssue(t, "PR A", 3*time.Hour)
	b := f.reviewIssue(t, "PR B", 2*time.Hour)
	c := f.reviewIssue(t, "PR C", 1*time.Hour)
	f.run(t, a, "running")
	for _, id := range []string{b, c} {
		if err := f.guard(id); !errors.Is(err, ErrStageQueueFull) {
			t.Fatalf("%s while A runs: err = %v, want ErrStageQueueFull", id, err)
		}
	}
	ws := util.MustParseUUID(f.WorkspaceID)
	load := func() []FullStage {
		got, err := LoadFullStages(context.Background(), f.q, ws)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := load()
	if len(got) != 1 || got[0].Stage != "review" || got[0].AgentID != f.agentID || got[0].Limit != 1 {
		t.Fatalf("full stages = %+v, want one review stage owned by the lane owner, limit 1", got)
	}
	if q := got[0].Queued; len(q) != 2 || q[0] != (QueuedPR{b, false}) || q[1] != (QueuedPR{c, false}) {
		t.Fatalf("queued = %+v, want B then C, no push task", q)
	}

	// An author's run on queued PR B is its open push task. The owner's own run does not count.
	rt := f.Runtime(t, "Author runtime")
	author := f.Agent(t, "Author", rt)
	f.Task(t, author, testutil.Cols{"issue_id": b, "status": "running", "runtime_id": rt})
	if q := load()[0].Queued; !q[0].PushOpen || q[1].PushOpen {
		t.Fatalf("queued = %+v, want push task open on B only", q)
	}

	// Another workspace sees nothing.
	other, err := LoadFullStages(context.Background(), f.q, util.MustParseUUID(f.Fixture.Workspace(t, "Other", fmt.Sprintf("other-%d", time.Now().UnixNano()))))
	if err != nil || len(other) != 0 {
		t.Fatalf("other workspace: %+v, %v; want none", other, err)
	}
}
