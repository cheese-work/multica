package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestEnqueueTaskForIssueConcurrentAssignAdmitsOnce is the C1 admission-boundary
// acceptance evidence for CHE-486: concurrent duplicate enqueues against the
// SAME (issue, agent) pending slot — e.g. an assignee change and a status
// promotion both racing to start the same obligation — must settle on exactly
// one admitted task instead of fanning out into duplicates or surfacing a raw
// constraint violation.
func TestEnqueueTaskForIssueConcurrentAssignAdmitsOnce(t *testing.T) {
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

	const concurrency = 25
	var wg sync.WaitGroup
	results := make([]string, concurrency)
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			task, err := svc.EnqueueTaskForIssue(ctx, issueStruct)
			if err != nil {
				errs[idx] = err
				return
			}
			results[idx] = util.UUIDToString(task.ID)
		}(i)
	}
	wg.Wait()

	admitted := ""
	admittedCount := 0
	coalescedCount := 0
	for i, err := range errs {
		if err != nil {
			if !errors.Is(err, ErrDuplicatePendingTask) {
				t.Fatalf("concurrent assign enqueue %d failed with an untyped/unexpected error: %v", i, err)
			}
			coalescedCount++
			continue
		}
		admittedCount++
		if admitted == "" {
			admitted = results[i]
		} else if results[i] != admitted {
			t.Errorf("concurrent assign enqueue %d admitted a second distinct task %s, want the single winner %s", i, results[i], admitted)
		}
	}
	if admittedCount != 1 {
		t.Errorf("expected exactly one admitted task across %d concurrent assign enqueues, got %d admitted / %d coalesced", concurrency, admittedCount, coalescedCount)
	}

	var liveCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')`,
		issueID, agentID).Scan(&liveCount); err != nil {
		t.Fatalf("count pending tasks: %v", err)
	}
	if liveCount != 1 {
		t.Errorf("expected exactly one pending task after %d concurrent assign enqueues, got %d", concurrency, liveCount)
	}
}

// TestEnqueueTaskForIssueDistinctIssuesRemainEligible proves the C1 admission
// fix does not over-suppress: two distinct issues assigned to the same agent
// are two distinct obligations and both must be admitted, even when enqueued
// concurrently.
func TestEnqueueTaskForIssueDistinctIssuesRemainEligible(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, actorID, agentID, issueID := seedAttributionFixture(t, pool)

	var otherIssueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id, assignee_type, assignee_id, priority, number)
		VALUES ($1, 'attr issue 2', 'member', $2, 'agent', $3, 'medium', 2)
		RETURNING id`, workspaceID, actorID, agentID).Scan(&otherIssueID); err != nil {
		t.Fatalf("seed second issue: %v", err)
	}

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}

	first, err := svc.EnqueueTaskForIssue(ctx, db.Issue{
		ID:           util.MustParseUUID(issueID),
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(actorID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	})
	if err != nil {
		t.Fatalf("EnqueueTaskForIssue (first issue): %v", err)
	}

	second, err := svc.EnqueueTaskForIssue(ctx, db.Issue{
		ID:           util.MustParseUUID(otherIssueID),
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(actorID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	})
	if err != nil {
		t.Fatalf("EnqueueTaskForIssue (second issue): %v", err)
	}

	if util.UUIDToString(first.ID) == util.UUIDToString(second.ID) {
		t.Fatalf("two distinct issues assigned to the same agent must not collapse into one task")
	}
}

// TestDispatchIssueRunCoalesceIsNotLogged is a narrow regression guard for the
// handler-level fix (CHE-486): dispatchIssueRun used to discard the enqueue
// error unconditionally (`_, _ =`), so both a benign coalesce AND a genuine
// enqueue failure vanished silently. This test only exercises the service-side
// typed error the handler now branches on — verifying enqueueIssueTask itself
// returns the typed sentinel rather than an opaque error is what makes the
// handler's errors.Is(err, ErrDuplicatePendingTask) check meaningful.
func TestEnqueueTaskForIssueSecondCallReturnsTypedDuplicateError(t *testing.T) {
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

	if _, err := svc.EnqueueTaskForIssue(ctx, issueStruct); err != nil {
		t.Fatalf("EnqueueTaskForIssue (first): %v", err)
	}

	_, err := svc.EnqueueTaskForIssue(ctx, issueStruct)
	if err == nil {
		t.Fatalf("expected the second enqueue against the same pending slot to be rejected")
	}
	if !errors.Is(err, ErrDuplicatePendingTask) {
		t.Fatalf("expected ErrDuplicatePendingTask, got a different/untyped error: %v", err)
	}
	// The error message must never leak the raw constraint name (#5914).
	if got := err.Error(); got != ErrDuplicatePendingTask.Error() {
		t.Errorf("duplicate error must be the bare sentinel, got %q", got)
	}
}
