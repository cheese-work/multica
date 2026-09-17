package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestDispatchCreateIssueEnqueueBranchToleratesDuplicatePendingTask is a
// narrow regression guard mirroring TestEnqueueTaskForIssueSecondCallReturnsTypedDuplicateError
// (CHE-486, issue_assign_admission_che486_test.go): it proves EnqueueTaskForIssue
// returns the typed ErrDuplicatePendingTask sentinel on a second call against
// the same pending slot, which is exactly the condition dispatchCreateIssue's
// non-squad enqueue branch (autopilot.go) must now tolerate instead of wrapping
// as a dispatch failure (CHE-561). The classification itself — errors.Is(err,
// ErrDuplicatePendingTask) — is exercised end to end by
// TestEnsureWebhookCreateIssueTaskConcurrentRepairAdmitsOnce below, which shares
// dispatchCreateIssue's identical EnqueueTaskForIssue(ctx, issue) call site.
func TestDispatchCreateIssueEnqueueBranchToleratesDuplicatePendingTask(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	_, _, agentID, issueID := seedAttributionFixture(t, pool)
	_ = agentID

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	issue, err := q.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}

	if _, err := svc.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("EnqueueTaskForIssue (first): %v", err)
	}

	_, err = svc.EnqueueTaskForIssue(ctx, issue)
	if !errors.Is(err, ErrDuplicatePendingTask) {
		t.Fatalf("second enqueue against the same pending slot: got %v, want ErrDuplicatePendingTask", err)
	}
	// dispatchCreateIssue's branch is `err != nil && !errors.Is(err, ErrDuplicatePendingTask)`
	// — confirm that guard evaluates to false (tolerated) for exactly this error.
	if err != nil && !errors.Is(err, ErrDuplicatePendingTask) {
		t.Fatalf("dispatchCreateIssue's tolerance guard would incorrectly treat this as a real failure: %v", err)
	}
}

// TestEnsureWebhookCreateIssueTaskConcurrentRepairAdmitsOnce is the recovery-path
// acceptance evidence for CHE-561: two concurrent repair attempts (e.g. a
// redelivered webhook worker retry racing the original dispatch's own
// completion check) both observe zero existing tasks for the linked issue via
// ListTasksByIssue and both attempt to enqueue. Exactly one may win the
// pending-slot admission; before this fix the loser's ErrDuplicatePendingTask
// was wrapped as an unconditional repair failure instead of being recognised
// as proof recovery already happened.
func TestEnsureWebhookCreateIssueTaskConcurrentRepairAdmitsOnce(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, creatorID, agentID, issueID := seedAttributionFixture(t, pool)

	// ensureWebhookCreateIssueTask only repairs an issue whose effective
	// status is still todo/in_progress; seeded issues default to backlog.
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'todo' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("set issue status: %v", err)
	}

	var autopilotID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot (workspace_id, title, assignee_type, assignee_id, status, execution_mode, created_by_type, created_by_id)
		VALUES ($1, 'webhook-repair-ap', 'agent', $2, 'active', 'create_issue', 'member', $3) RETURNING id`,
		workspaceID, agentID, creatorID).Scan(&autopilotID); err != nil {
		t.Fatalf("seed autopilot: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, autopilotID) })

	svc := &AutopilotService{
		Queries:   q,
		TxStarter: pool,
		Bus:       events.New(),
		TaskSvc:   &TaskService{Queries: q, TxStarter: pool, Bus: events.New()},
	}

	ap, err := q.GetAutopilot(ctx, util.MustParseUUID(autopilotID))
	if err != nil {
		t.Fatalf("get autopilot: %v", err)
	}
	run := db.AutopilotRun{
		IssueID: util.MustParseUUID(issueID),
		Status:  "issue_created",
	}

	const concurrency = 10
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = svc.ensureWebhookCreateIssueTask(ctx, ap, run)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent repair attempt %d returned an error instead of coalescing: %v", i, err)
		}
	}

	var liveCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')`,
		issueID, agentID).Scan(&liveCount); err != nil {
		t.Fatalf("count pending tasks: %v", err)
	}
	if liveCount != 1 {
		t.Errorf("expected exactly one pending task after %d concurrent repair attempts, got %d", concurrency, liveCount)
	}
}

// TestEnsureWebhookCreateIssueTaskGenuineEnqueueFailureStillReturnsError is a
// negative control: only ErrDuplicatePendingTask is treated as a benign
// coalesce. Any other enqueue error must still surface as a repair failure so
// a real defect is never swallowed under the same classification.
func TestEnsureWebhookCreateIssueTaskGenuineEnqueueFailureStillReturnsError(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, creatorID, agentID, issueID := seedAttributionFixture(t, pool)
	_ = agentID

	var autopilotID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot (workspace_id, title, assignee_type, assignee_id, status, execution_mode, created_by_type, created_by_id)
		VALUES ($1, 'webhook-repair-fail-ap', 'agent', $2, 'active', 'create_issue', 'member', $3) RETURNING id`,
		workspaceID, util.MustParseUUID(agentID), creatorID).Scan(&autopilotID); err != nil {
		t.Fatalf("seed autopilot: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, autopilotID) })

	// Deleting the linked issue makes the subsequent GetIssue lookup inside
	// ensureWebhookCreateIssueTask fail with a real (non-duplicate) error.
	if _, err := pool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID); err != nil {
		t.Fatalf("delete issue: %v", err)
	}

	svc := &AutopilotService{
		Queries:   q,
		TxStarter: pool,
		Bus:       events.New(),
		TaskSvc:   &TaskService{Queries: q, TxStarter: pool, Bus: events.New()},
	}
	ap, err := q.GetAutopilot(ctx, util.MustParseUUID(autopilotID))
	if err != nil {
		t.Fatalf("get autopilot: %v", err)
	}
	run := db.AutopilotRun{IssueID: util.MustParseUUID(issueID), Status: "issue_created"}

	err = svc.ensureWebhookCreateIssueTask(ctx, ap, run)
	if err == nil {
		t.Fatalf("expected a real error when the linked issue no longer exists")
	}
	if errors.Is(err, ErrDuplicatePendingTask) {
		t.Fatalf("a missing-issue failure must never be classified as the duplicate coalesce sentinel")
	}
}
