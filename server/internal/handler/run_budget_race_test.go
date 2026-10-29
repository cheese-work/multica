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
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	barrier := &admissionReadBarrier{DBTX: testPool, ready: ready, release: release}
	tasks := service.NewTaskService(db.New(barrier), testPool, testHandler.TaskService.Hub, testHandler.TaskService.Bus)
	results := make(chan error, 2)
	for _, commentID := range commentIDs {
		go func(commentID string) {
			_, err := tasks.EnqueueTaskForMention(ctx, issue, issue.AssigneeID, parseUUID(commentID), service.OriginDerived)
			results <- err
		}(commentID)
	}
	for range 2 {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("callers did not reach the first-review read")
		}
	}
	close(release)
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
	ready := make(chan struct{}, 2)
	releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
	results := make(chan error, 2)
	for _, release := range releases {
		barrier := &admissionReadBarrier{DBTX: testPool, ready: ready, release: release}
		tasks := service.NewTaskService(db.New(barrier), testPool, testHandler.TaskService.Hub, testHandler.TaskService.Bus)
		go func() {
			_, err := tasks.EnqueueTaskForIssue(ctx, issue)
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("callers did not reach the first-review read")
		}
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("first actual enqueue: %v", err)
	}
	admitted := 1
	for _, release := range releases {
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=clock_timestamp() WHERE issue_id=$1 AND status='queued'`, fixture.issueID)
		close(release)
		if err := <-results; err == nil {
			admitted++
		} else {
			t.Logf("stale caller refused: %v", err)
		}
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
