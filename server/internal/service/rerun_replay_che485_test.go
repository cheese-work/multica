package service

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestRerunIssueReplayReturnsExistingTask is the acceptance test for CHE-485:
// an authorized force (rerun) that is replayed — the same task_id, the same
// actor — must NOT duplicate the admitted run. The second call is a duplicate
// delivery of the same rerun request (double-click, retried HTTP call,
// duplicate webhook), and must return the exact task the first call produced
// instead of cancelling it and enqueueing a third row.
func TestRerunIssueReplayReturnsExistingTask(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, actorID, agentID, issueID := seedAttributionFixture(t, pool)

	issueStruct := db.Issue{
		ID:           util.MustParseUUID(issueID),
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(actorID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	}
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	orig, err := svc.EnqueueTaskForIssue(ctx, issueStruct)
	if err != nil {
		t.Fatalf("EnqueueTaskForIssue (original): %v", err)
	}

	countTasks := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&n); err != nil {
			t.Fatalf("count tasks: %v", err)
		}
		return n
	}

	allow := func(db.Agent) bool { return true }
	actorUUID := util.MustParseUUID(actorID)

	// First rerun: the authorized force action. Cancels the original pending
	// task and enqueues a fresh one.
	first, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), orig.ID, pgtype.UUID{}, actorUUID, allow)
	if err != nil {
		t.Fatalf("RerunIssue (first): %v", err)
	}
	if util.UUIDToString(first.ID) == util.UUIDToString(orig.ID) {
		t.Fatalf("expected a new task id on first rerun, got the original %s", util.UUIDToString(orig.ID))
	}
	afterFirst := countTasks()

	// Replay: identical request (same source task_id, same actor) submitted
	// again before the rerun task has settled. Must return the SAME task,
	// with zero additional rows and zero additional cancellations.
	replay, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), orig.ID, pgtype.UUID{}, actorUUID, allow)
	if err != nil {
		t.Fatalf("RerunIssue (replay): %v", err)
	}
	if util.UUIDToString(replay.ID) != util.UUIDToString(first.ID) {
		t.Errorf("replay produced a different task: got %s, want the first rerun's task %s",
			util.UUIDToString(replay.ID), util.UUIDToString(first.ID))
	}
	if got := countTasks(); got != afterFirst {
		t.Errorf("replay changed task count: got %d, want %d (no new row)", got, afterFirst)
	}
	firstStatus := ""
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, first.ID).Scan(&firstStatus); err != nil {
		t.Fatalf("read first rerun status: %v", err)
	}
	if firstStatus == "cancelled" {
		t.Errorf("replay cancelled the live rerun task instead of returning it")
	}

	// A different actor rerunning the SAME source task is a distinct
	// obligation (a different human authorizing the same force action), not a
	// replay, and must still be admitted normally.
	var otherActorID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Other Actor', $1) RETURNING id`,
		"che485-other-actor@multica.test").Scan(&otherActorID); err != nil {
		t.Fatalf("seed other actor: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, otherActorID) })
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'admin')`,
		workspaceID, otherActorID); err != nil {
		t.Fatalf("seed other actor member: %v", err)
	}
	distinct, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), orig.ID, pgtype.UUID{}, util.MustParseUUID(otherActorID), allow)
	if err != nil {
		t.Fatalf("RerunIssue (distinct actor): %v", err)
	}
	if util.UUIDToString(distinct.ID) == util.UUIDToString(first.ID) {
		t.Errorf("a different actor's rerun of the same source task must not collapse into the first actor's replay")
	}
}

// TestRerunIssueReplayAfterTerminalAdmitsFreshAttempt proves the replay guard
// does not become permanent suppression: once the rerun task this request
// would replay has reached a terminal state, a failed or cancelled attempt is
// not a fulfilled obligation, and a fresh rerun of the same source task by the
// same actor must be admitted as a new attempt, not silently dropped.
func TestRerunIssueReplayAfterTerminalAdmitsFreshAttempt(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, actorID, agentID, issueID := seedAttributionFixture(t, pool)

	issueStruct := db.Issue{
		ID:           util.MustParseUUID(issueID),
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(actorID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	}
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	orig, err := svc.EnqueueTaskForIssue(ctx, issueStruct)
	if err != nil {
		t.Fatalf("EnqueueTaskForIssue (original): %v", err)
	}

	allow := func(db.Agent) bool { return true }
	actorUUID := util.MustParseUUID(actorID)

	first, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), orig.ID, pgtype.UUID{}, actorUUID, allow)
	if err != nil {
		t.Fatalf("RerunIssue (first): %v", err)
	}

	// Settle the rerun task as failed — a failed attempt is not a fulfilled
	// obligation and must not keep shadowing a fresh rerun of the same source.
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'failed' WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("settle first rerun as failed: %v", err)
	}

	again, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), orig.ID, pgtype.UUID{}, actorUUID, allow)
	if err != nil {
		t.Fatalf("RerunIssue (after terminal): %v", err)
	}
	if util.UUIDToString(again.ID) == util.UUIDToString(first.ID) {
		t.Errorf("a rerun after the prior attempt failed must admit a fresh task, not return the terminal one")
	}
}

// TestRerunIssueReplayConcurrentSubmissionsAdmitOnce is the concurrency
// acceptance evidence for CHE-485: N concurrent replays of the same rerun
// request (same source task, same actor) must settle on exactly one
// surviving live task for that lineage — concurrent duplicate submissions
// never fan out into multiple admitted runs.
func TestRerunIssueReplayConcurrentSubmissionsAdmitOnce(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, actorID, agentID, issueID := seedAttributionFixture(t, pool)

	issueStruct := db.Issue{
		ID:           util.MustParseUUID(issueID),
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(actorID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	}
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	orig, err := svc.EnqueueTaskForIssue(ctx, issueStruct)
	if err != nil {
		t.Fatalf("EnqueueTaskForIssue (original): %v", err)
	}

	allow := func(db.Agent) bool { return true }
	actorUUID := util.MustParseUUID(actorID)

	// Seed the first admitted rerun outside the race so every concurrent
	// caller below is a pure replay against an already-live lineage — this
	// isolates the replay guard's race behavior from the initial admission's.
	first, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), orig.ID, pgtype.UUID{}, actorUUID, allow)
	if err != nil {
		t.Fatalf("RerunIssue (seed): %v", err)
	}

	const concurrency = 25
	var wg sync.WaitGroup
	results := make([]string, concurrency)
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			task, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), orig.ID, pgtype.UUID{}, actorUUID, allow)
			if err != nil {
				errs[idx] = err
				return
			}
			results[idx] = util.UUIDToString(task.ID)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent replay %d failed: %v", i, err)
		}
	}
	for i, id := range results {
		if id != util.UUIDToString(first.ID) {
			t.Errorf("concurrent replay %d returned task %s, want the single live rerun %s", i, id, util.UUIDToString(first.ID))
		}
	}

	var liveCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE rerun_of_task_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')`,
		orig.ID).Scan(&liveCount); err != nil {
		t.Fatalf("count live reruns: %v", err)
	}
	if liveCount != 1 {
		t.Errorf("expected exactly one live rerun task after %d concurrent replays, got %d", concurrency, liveCount)
	}
}

// TestRerunIssueConcurrentFirstAdmissionSettlesOnce is the first-admission
// race regression for CHE-485: unlike
// TestRerunIssueReplayConcurrentSubmissionsAdmitOnce, no rerun is seeded
// before the goroutines start, so every caller races
// FindLiveRerunOfTask's read against an EMPTY lineage — the exact window
// where multiple callers can both see "no live rerun yet" and both reach the
// insert. All goroutines share ONE trigger comment (pgtype.UUID{}, matching
// what real duplicate-rerun HTTP replays share), so they also all contend the
// SAME idx_one_pending_task_per_issue_agent_thread slot at once — exercising
// N-way contention on both unique indexes together, exactly as production
// duplicate submissions do. idx_one_live_rerun_per_source_task_actor
// (migration 504) must reject every insert but one, and the retry/resolution
// logic in RerunIssue must settle every caller — winners and losers alike —
// on the single admitted task instead of erroring or leaving it cancelled.
func TestRerunIssueConcurrentFirstAdmissionSettlesOnce(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, actorID, agentID, issueID := seedAttributionFixture(t, pool)

	issueStruct := db.Issue{
		ID:           util.MustParseUUID(issueID),
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(actorID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	}
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	orig, err := svc.EnqueueTaskForIssue(ctx, issueStruct)
	if err != nil {
		t.Fatalf("EnqueueTaskForIssue (original): %v", err)
	}

	allow := func(db.Agent) bool { return true }
	actorUUID := util.MustParseUUID(actorID)

	const concurrency = 25
	var wg sync.WaitGroup
	results := make([]string, concurrency)
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			task, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), orig.ID, pgtype.UUID{}, actorUUID, allow)
			if err != nil {
				errs[idx] = err
				return
			}
			results[idx] = util.UUIDToString(task.ID)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent first-admission rerun %d failed: %v", i, err)
		}
	}

	winner := results[0]
	if winner == "" {
		t.Fatalf("first result was empty")
	}
	for i, id := range results {
		if id != winner {
			t.Errorf("concurrent rerun %d returned task %s, want the single admitted winner %s", i, id, winner)
		}
	}

	var liveCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE rerun_of_task_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')`,
		orig.ID).Scan(&liveCount); err != nil {
		t.Fatalf("count live reruns: %v", err)
	}
	if liveCount != 1 {
		t.Errorf("expected exactly one live rerun task after %d concurrent first admissions, got %d", concurrency, liveCount)
	}

	var winnerStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, winner).Scan(&winnerStatus); err != nil {
		t.Fatalf("read winner status: %v", err)
	}
	if winnerStatus == "cancelled" {
		t.Errorf("the task every caller settled on must not itself be cancelled")
	}
}
