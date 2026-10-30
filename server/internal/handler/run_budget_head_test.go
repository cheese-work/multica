package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRunBudgetPreparedMentionAdmitsTheHeadItIsStampedWith(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	fixture := newEdgeCase(t, 2226401, mergeEdgeSteps)
	issue := spendBudget(t, fixture, 2, "in_review")
	linkPR(t, fixture.issueID, 2226401, "head-prepared-old")
	params, err := testHandler.TaskService.PrepareTaskForMention(ctx, testHandler.Queries, issue, issue.AssigneeID, pgtype.UUID{}, pgtype.UUID{}, service.OriginDerived)
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha='head-prepared-new' WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)`, fixture.issueID)
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := testHandler.TaskService.EnqueuePreparedTaskForMentionInTx(ctx, testHandler.Queries.WithTx(tx), issue, params, service.OriginDerived); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	oldClaims := dbfx.Count(t, `SELECT count(*) FROM issue_review_admission WHERE issue_id=$1 AND head_sha='head-prepared-old'`, fixture.issueID)
	newClaims := dbfx.Count(t, `SELECT count(*) FROM issue_review_admission WHERE issue_id=$1 AND head_sha='head-prepared-new'`, fixture.issueID)
	oldTasks := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'head_sha'='head-prepared-old'`, fixture.issueID)
	newTasks := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'head_sha'='head-prepared-new'`, fixture.issueID)
	t.Logf("after prepared insert: old_claims=%d new_claims=%d old_tasks=%d new_tasks=%d", oldClaims, newClaims, oldTasks, newTasks)
	mismatches := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue task WHERE issue_id=$1 AND context->>'head_sha' IS NOT NULL AND NOT EXISTS (SELECT 1 FROM issue_review_admission admission WHERE admission.issue_id=task.issue_id AND admission.head_sha=task.context->>'head_sha')`, fixture.issueID)
	if mismatches != 0 {
		t.Errorf("admission does not match the run's pinned head: %d task(s)", mismatches)
	}
	finishRuns(t, fixture)
	_, err = testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
	settle()
	t.Logf("first actual new-head review: error=%v status=%s", err, fixture.status(t))
	if newTasks == 0 && err != nil {
		t.Errorf("new-head admission spent without a new-head task: %v", err)
	}
}

func TestRunBudgetHeadAdvanceBetweenGuardAndInsertKeepsOneHead(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newEdgeCase(t, 2226402, mergeEdgeSteps)
	issue := spendBudget(t, fixture, 2, "in_review")
	linkPR(t, fixture.issueID, 2226402, "head-race-old")
	ready := make(chan struct{}, 1)
	release := make(chan struct{})
	barrier := &admissionReadBarrier{DBTX: testPool, ready: ready, release: release}
	tasks := service.NewTaskService(db.New(barrier), admissionTxStarter{barrier}, testHandler.TaskService.Hub, testHandler.TaskService.Bus)
	result := make(chan error, 1)
	go func() {
		_, err := tasks.EnqueueTaskForIssue(ctx, issue)
		result <- err
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("enqueue did not reach the admission read")
	}
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha='head-race-new' WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)`, fixture.issueID)
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	oldClaims := dbfx.Count(t, `SELECT count(*) FROM issue_review_admission WHERE issue_id=$1 AND head_sha='head-race-old'`, fixture.issueID)
	newClaims := dbfx.Count(t, `SELECT count(*) FROM issue_review_admission WHERE issue_id=$1 AND head_sha='head-race-new'`, fixture.issueID)
	newTasks := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'head_sha'='head-race-new'`, fixture.issueID)
	t.Logf("after head advance: old_claims=%d new_claims=%d new_tasks=%d", oldClaims, newClaims, newTasks)
	finishRuns(t, fixture)
	_, enqueueErr := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
	newTasks = dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'head_sha'='head-race-new'`, fixture.issueID)
	t.Logf("after second same-head request: new_tasks=%d error=%v", newTasks, enqueueErr)
	if oldClaims != 1 || newTasks != 1 {
		t.Fatalf("head advance: old_claims=%d new_tasks=%d, want the first run to claim and carry one head and the next head's review to be admitted once", oldClaims, newTasks)
	}
}
