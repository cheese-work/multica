package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8749 S7a: every claim poll on the agent, runtime and batch paths adds one
// count to an in-process histogram that is flushed to claim_poll_stats.

func pollCount(snap map[ClaimPollKey]ClaimPollEntry, path, outcome string) int64 {
	return snap[ClaimPollKey{Path: path, Outcome: outcome}].Count
}

func pollTotal(snap map[ClaimPollKey]ClaimPollEntry) int64 {
	var n int64
	for _, e := range snap {
		n += e.Count
	}
	return n
}

func TestClaimPollStatsObserveCountsEachOutcomeOnce(t *testing.T) {
	stats := NewClaimPollStats()

	stats.Observe(claimPathRuntime, ClaimPollOutcomeEmpty, 5*time.Millisecond)
	stats.Observe(claimPathRuntime, ClaimPollOutcomeError, 40*time.Millisecond)
	stats.Observe(claimPathRuntime, ClaimPollOutcomeRankWalkExhausted, 2*time.Second)

	snap := stats.Snapshot()
	for _, outcome := range []string{ClaimPollOutcomeEmpty, ClaimPollOutcomeError, ClaimPollOutcomeRankWalkExhausted} {
		if got := pollCount(snap, claimPathRuntime, outcome); got != 1 {
			t.Errorf("count{runtime,%s} = %d, want 1", outcome, got)
		}
	}
	if got := pollTotal(snap); got != 3 {
		t.Errorf("total polls = %d, want 3", got)
	}

	empty := snap[ClaimPollKey{Path: claimPathRuntime, Outcome: ClaimPollOutcomeEmpty}]
	if empty.SumMs != 5 {
		t.Errorf("sum_ms of the 5 ms empty poll = %v, want 5", empty.SumMs)
	}
	// Bucket bounds are inclusive upper bounds: 5 ms lands in the "<= 5 ms" bucket.
	var inBucket int
	for i, n := range empty.Buckets {
		if n == 0 {
			continue
		}
		inBucket++
		if i >= len(ClaimPollBucketBoundsMs) || ClaimPollBucketBoundsMs[i] != 5 {
			t.Errorf("5 ms poll counted in bucket %d, want the <= 5 ms bucket", i)
		}
	}
	if inBucket != 1 {
		t.Errorf("5 ms poll counted in %d buckets, want exactly 1", inBucket)
	}
	slow := snap[ClaimPollKey{Path: claimPathRuntime, Outcome: ClaimPollOutcomeRankWalkExhausted}]
	if last := slow.Buckets[len(slow.Buckets)-1]; last != 1 {
		t.Errorf("2 s poll overflow bucket = %d, want 1", last)
	}
}

func TestClaimPollStatsFlushAccumulatesAndResets(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	q := db.New(pool)
	stats := NewClaimPollStats()
	minute := time.Now().UTC().Truncate(time.Minute).Add(-24 * time.Hour * 365 * 30) // far past: no clash with other runs
	path := fmt.Sprintf("flush-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM claim_poll_stats WHERE path = $1`, path) })

	stats.Observe(path, ClaimPollOutcomeEmpty, 5*time.Millisecond)
	stats.Observe(path, ClaimPollOutcomeEmpty, 30*time.Millisecond)
	if err := stats.Flush(ctx, q, minute); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := pollTotal(stats.Snapshot()); got != 0 {
		t.Fatalf("snapshot after flush holds %d polls, want 0", got)
	}

	// A second process (or a second flush in the same minute) adds to the row.
	stats.Observe(path, ClaimPollOutcomeEmpty, 5*time.Millisecond)
	if err := stats.Flush(ctx, q, minute); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	var count int64
	var sumMs float64
	var buckets []int64
	if err := pool.QueryRow(ctx, `SELECT count, sum_ms, bucket_counts FROM claim_poll_stats
		WHERE minute = $1 AND path = $2 AND outcome = $3`, minute, path, ClaimPollOutcomeEmpty).Scan(&count, &sumMs, &buckets); err != nil {
		t.Fatalf("read claim_poll_stats: %v", err)
	}
	if count != 3 || sumMs != 40 {
		t.Errorf("row = count %d sum_ms %v, want 3 and 40", count, sumMs)
	}
	var inBuckets int64
	for _, n := range buckets {
		inBuckets += n
	}
	if inBuckets != 3 || len(buckets) != len(ClaimPollBucketBoundsMs)+1 {
		t.Errorf("bucket_counts = %v, want %d slots holding 3", buckets, len(ClaimPollBucketBoundsMs)+1)
	}
}

type failingExecDBTX struct{ db.DBTX }

func (failingExecDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("injected flush failure")
}

func (f failingExecDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("injected flush failure")
}

func (f failingExecDBTX) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return failingRow{}
}

type failingRow struct{}

func (failingRow) Scan(...any) error { return errors.New("injected flush failure") }

func TestClaimPollStatsFlushFailureKeepsCounts(t *testing.T) {
	stats := NewClaimPollStats()
	stats.Observe(claimPathBatch, ClaimPollOutcomeClaimed, 12*time.Millisecond)

	if err := stats.Flush(context.Background(), db.New(failingExecDBTX{}), time.Now()); err == nil {
		t.Fatal("flush against a failing database returned nil")
	}
	if got := pollCount(stats.Snapshot(), claimPathBatch, ClaimPollOutcomeClaimed); got != 1 {
		t.Fatalf("count after failed flush = %d, want 1 kept for the next flush", got)
	}
}

// pollFixture is one runtime with agents, a service whose poll stats the test
// reads, and helpers to queue one task per agent.
type pollFixture struct {
	svc       *TaskService
	fx        *testutil.Fixture
	runtimeID pgtype.UUID
}

func newPollFixture(t *testing.T) *pollFixture {
	t.Helper()
	pool := sharedTestPool(t)
	bootstrap := testutil.New(pool, "", "")
	suffix := time.Now().UnixNano()
	ownerID := bootstrap.User(t, fmt.Sprintf("poll-stats-owner-%d", suffix), fmt.Sprintf("poll-stats-owner-%d@example.com", suffix))
	workspaceID := bootstrap.Workspace(t, fmt.Sprintf("poll-stats-%d", suffix), fmt.Sprintf("poll-stats-%d", suffix))
	fx := testutil.New(pool, workspaceID, ownerID)
	fx.Member(t, workspaceID, ownerID, "owner")
	runtimeID := fx.Runtime(t, "poll-stats-runtime", testutil.Cols{"visibility": "public", "owner_id": ownerID})
	return &pollFixture{
		svc:       NewTaskService(db.New(pool), pool, nil, events.New()),
		fx:        fx,
		runtimeID: util.MustParseUUID(runtimeID),
	}
}

// queueTask adds an agent (max_concurrent_tasks = maxConcurrent) with one queued task.
func (f *pollFixture) queueTask(t *testing.T, name string, maxConcurrent int, priority int) (agentID string) {
	t.Helper()
	runtimeID := util.UUIDToString(f.runtimeID)
	agentID = f.fx.Agent(t, name, runtimeID, testutil.Cols{"max_concurrent_tasks": maxConcurrent})
	issueID := f.fx.Issue(t, name+" issue")
	f.fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "priority": priority})
	return agentID
}

func TestClaimPollStatsRuntimePathCountsEmptyAndClaimedPolls(t *testing.T) {
	f := newPollFixture(t)
	ctx := context.Background()

	if task, err := f.svc.ClaimTaskForRuntime(ctx, f.runtimeID); err != nil || task != nil {
		t.Fatalf("empty poll = (%v, %v), want (nil, nil)", task, err)
	}
	snap := f.svc.PollStats.Snapshot()
	if got := pollCount(snap, claimPathRuntime, ClaimPollOutcomeEmpty); got != 1 || pollTotal(snap) != 1 {
		t.Fatalf("after an empty poll: empty=%d total=%d, want 1 and 1", got, pollTotal(snap))
	}

	f.queueTask(t, "poll-stats-claimer", 5, 0)
	if task, err := f.svc.ClaimTaskForRuntime(ctx, f.runtimeID); err != nil || task == nil {
		t.Fatalf("claiming poll = (%v, %v), want a task", task, err)
	}
	snap = f.svc.PollStats.Snapshot()
	if got := pollCount(snap, claimPathRuntime, ClaimPollOutcomeClaimed); got != 1 || pollTotal(snap) != 2 {
		t.Fatalf("after a claiming poll: claimed=%d total=%d, want 1 and 2", got, pollTotal(snap))
	}
}

func TestClaimPollStatsAgentPathCountsOnePoll(t *testing.T) {
	f := newPollFixture(t)
	agentID := f.queueTask(t, "poll-stats-agent-path", 5, 0)

	if task, err := f.svc.ClaimTask(context.Background(), util.MustParseUUID(agentID)); err != nil || task == nil {
		t.Fatalf("ClaimTask = (%v, %v), want a task", task, err)
	}
	snap := f.svc.PollStats.Snapshot()
	if got := pollCount(snap, claimPathAgent, ClaimPollOutcomeClaimed); got != 1 || pollTotal(snap) != 1 {
		t.Fatalf("agent path: claimed=%d total=%d, want 1 and 1", got, pollTotal(snap))
	}
}

func TestClaimPollStatsRefusedPollIsCounted(t *testing.T) {
	f := newPollFixture(t)
	f.queueTask(t, "poll-stats-no-capacity", 0, 0) // visible candidate, capacity cap refuses it

	if task, err := f.svc.ClaimTaskForRuntime(context.Background(), f.runtimeID); err != nil || task != nil {
		t.Fatalf("refused poll = (%v, %v), want (nil, nil)", task, err)
	}
	snap := f.svc.PollStats.Snapshot()
	if got := pollCount(snap, claimPathRuntime, ClaimPollOutcomeRefused); got != 1 || pollTotal(snap) != 1 {
		t.Fatalf("refused=%d total=%d, want 1 and 1", pollCount(snap, claimPathRuntime, ClaimPollOutcomeRefused), pollTotal(snap))
	}
}

// A batch that claims 2 agents and refuses 3 is one poll, not five.
func TestClaimPollStatsBatchOfFiveAgentsIsOnePoll(t *testing.T) {
	f := newPollFixture(t)
	for i := range 2 {
		f.queueTask(t, fmt.Sprintf("poll-stats-batch-ok-%d", i), 5, 0)
	}
	for i := range 3 {
		f.queueTask(t, fmt.Sprintf("poll-stats-batch-refused-%d", i), 0, 0)
	}

	claimed, err := f.svc.ClaimTasksForRuntimes(context.Background(), []pgtype.UUID{f.runtimeID}, 10)
	if err != nil {
		t.Fatalf("ClaimTasksForRuntimes: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d tasks, want 2", len(claimed))
	}
	snap := f.svc.PollStats.Snapshot()
	if got := pollCount(snap, claimPathBatch, ClaimPollOutcomeClaimed); got != 1 {
		t.Errorf("count{batch,claimed} = %d, want 1", got)
	}
	if got := pollTotal(snap); got != 1 {
		t.Errorf("total polls = %d, want 1 (the batch is one poll)", got)
	}
}

// queue_order=priority is the default and equals today's behavior: the poll
// stats do not change which task a claim returns. The setting is read by the
// ordering slices (S4b); S7a measures in both modes and must not alter order.
func TestClaimOrderIsPriorityOrderWithAndWithoutQueueOrderSetting(t *testing.T) {
	claimOrder := func(t *testing.T, setting bool) []int32 {
		f := newPollFixture(t)
		if setting {
			if _, err := sharedTestPool(t).Exec(context.Background(),
				`UPDATE workspace SET settings = settings || '{"queue_order":"priority"}'::jsonb WHERE id = $1`, f.fx.WorkspaceID); err != nil {
				t.Fatalf("set queue_order: %v", err)
			}
		}
		runtimeID := util.UUIDToString(f.runtimeID)
		// One agent, two queued tasks on different issues: priority DESC decides.
		agentID := f.fx.Agent(t, "order-agent", runtimeID, testutil.Cols{"max_concurrent_tasks": 1})
		for _, p := range []int{1, 3} {
			f.fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": f.fx.Issue(t, fmt.Sprintf("order %d", p)), "priority": p})
		}
		var got []int32
		for range 2 {
			task, err := f.svc.ClaimTaskForRuntime(context.Background(), f.runtimeID)
			if err != nil || task == nil {
				t.Fatalf("claim = (%v, %v)", task, err)
			}
			got = append(got, task.Priority)
			// Free the capacity slot so the second claim can run.
			if _, err := sharedTestPool(t).Exec(context.Background(),
				`UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, task.ID); err != nil {
				t.Fatalf("free slot: %v", err)
			}
		}
		return got
	}

	def := claimOrder(t, false)
	set := claimOrder(t, true)
	if fmt.Sprint(def) != "[3 1]" || fmt.Sprint(set) != fmt.Sprint(def) {
		t.Fatalf("claim order default=%v with queue_order=priority=%v, want [3 1] both", def, set)
	}
}
