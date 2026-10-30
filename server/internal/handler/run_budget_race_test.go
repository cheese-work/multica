package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// admissionReadBarrier holds every caller between the guard's read of the
// head's admission and the insert, so concurrent enqueues all see "not yet
// reviewed" and only the insert itself can decide the winner.
type admissionReadBarrier struct {
	db.DBTX
	ready   chan<- struct{}
	release <-chan struct{}
}

type admissionReadRow struct {
	pgx.Row
	ctx     context.Context
	barrier *admissionReadBarrier
}

func (barrier *admissionReadBarrier) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	row := barrier.DBTX.QueryRow(ctx, query, args...)
	if strings.Contains(query, "-- name: HasIssueReviewAdmission") {
		return admissionReadRow{Row: row, ctx: ctx, barrier: barrier}
	}
	return row
}

func (row admissionReadRow) Scan(dest ...any) error {
	if err := row.Row.Scan(dest...); err != nil {
		return err
	}
	row.barrier.ready <- struct{}{}
	select {
	case <-row.barrier.release:
		return nil
	case <-row.ctx.Done():
		return row.ctx.Err()
	}
}

// admissionTxStarter puts the barrier inside the enqueue transaction: the guard
// reads through the transaction, not through the service's pool queries.
type admissionTxStarter struct{ barrier *admissionReadBarrier }

type admissionTx struct {
	pgx.Tx
	barrier *admissionReadBarrier
}

func (starter admissionTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := testPool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	inner := &admissionReadBarrier{DBTX: tx, ready: starter.barrier.ready, release: starter.barrier.release}
	return admissionTx{Tx: tx, barrier: inner}, nil
}

func (tx admissionTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return tx.barrier.QueryRow(ctx, query, args...)
}

func TestRunBudgetConcurrentThreadsAdmitOneReviewPerHead(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newEdgeCase(t, 2226191, mergeEdgeSteps)
	issue := spendBudget(t, fixture, 2, "in_review")
	linkPR(t, fixture.issueID, 2226191, "race-concurrent-threads")
	authorID := createHandlerTestAgent(t, "race-comment-author", nil)
	commentIDs := []string{
		dbfx.Comment(t, fixture.issueID, "review request one", testutil.Cols{"author_type": "agent", "author_id": authorID}),
		dbfx.Comment(t, fixture.issueID, "review request two", testutil.Cols{"author_type": "agent", "author_id": authorID}),
	}
	// The issue lock serializes the callers: the second reads after the
	// first's insert, so the head is admitted once without a barrier.
	results := make(chan error, 2)
	for _, commentID := range commentIDs {
		go func(commentID string) {
			_, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, issue.AssigneeID, parseUUID(commentID), service.OriginDerived)
			results <- err
		}(commentID)
	}
	admitted := 0
	for range 2 {
		if err := <-results; err == nil {
			admitted++
		} else {
			t.Logf("enqueue refused: %v", err)
		}
	}
	pending := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='queued' AND context->>'head_sha'='race-concurrent-threads'`, fixture.issueID)
	threads := dbfx.Count(t, `SELECT count(DISTINCT comment_thread_id) FROM agent_task_queue WHERE issue_id=$1 AND context->>'head_sha'='race-concurrent-threads'`, fixture.issueID)
	t.Logf("same issue, agent and head: admitted=%d queued=%d distinct_threads=%d; no run started or completed", admitted, pending, threads)
	if admitted != 1 || pending != 1 {
		t.Fatalf("first-review exception admitted %d runs for one head across threads, want 1", admitted)
	}
}

func TestRunBudgetStaleCallersAfterFirstReviewStartedAreRefused(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newEdgeCase(t, 2226192, mergeEdgeSteps)
	issue := spendBudget(t, fixture, 2, "in_review")
	linkPR(t, fixture.issueID, 2226192, "race-running-window")
	// The issue lock closes the window two stale readers shared: each caller
	// reads after the previous one's insert and start.
	admitted := 0
	for range 3 {
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err == nil {
			admitted++
		} else {
			t.Logf("stale caller refused: %v", err)
		}
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=clock_timestamp() WHERE issue_id=$1 AND status='queued'`, fixture.issueID)
	}
	runs := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'head_sha'='race-running-window'`, fixture.issueID)
	t.Logf("same thread, agent and head: admitted=%d stamped_rows=%d; no review completed", admitted, runs)
	if admitted != 1 || runs != 1 {
		t.Fatalf("stale first-review checks admitted %d runs for one head after starts, want 1", admitted)
	}
}

func TestRunBudgetEarlierCoordinationDoesNotSpendReviewAdmission(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	fixture := newEdgeCase(t, 2226193, mergeEdgeSteps)
	issue := spendBudget(t, fixture, 2, "in_progress")
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue=3 WHERE id=$1`, fixture.workflowID)
	// A cap edit reaches the issue when its next window opens, not an open one.
	dbfx.Exec(t, `DELETE FROM issue_run_budget_entry WHERE issue_id=$1`, fixture.issueID)
	linkPR(t, fixture.issueID, 2226193, "race-coordination-before-review")
	reviewerID := parseUUID(createHandlerTestAgent(t, "race-future-reviewer", nil))
	if _, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, reviewerID, pgtype.UUID{}, service.OriginDerived); err != nil {
		t.Fatalf("earlier coordination run: %v", err)
	}
	finishRuns(t, fixture)
	dbfx.Exec(t, `UPDATE issue SET status='in_review',assignee_id=$2 WHERE id=$1`, fixture.issueID, reviewerID)
	issue, err := testHandler.Queries.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, enqueueErr := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
	settle()
	coordination := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND workflow_step='in_progress' AND context->>'head_sha'='race-coordination-before-review'`, fixture.issueID, reviewerID)
	reviews := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND workflow_step='in_review'`, fixture.issueID, reviewerID)
	t.Logf("prior coordination=%d actual review rows=%d status=%s", coordination, reviews, fixture.status(t))
	if enqueueErr != nil {
		t.Fatalf("first review refused because reviewer had a non-review coordination run: %v", enqueueErr)
	}
}
