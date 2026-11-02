package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// exhaustedNotice returns the exhaustion notice comment text of the issue.
func (c budgetCase) exhaustedNotice(t *testing.T) string {
	t.Helper()
	var content string
	err := testPool.QueryRow(context.Background(), `SELECT c.content FROM run_budget_notice n JOIN comment c ON c.id=n.comment_id WHERE n.issue_id=$1 AND n.kind=$2`,
		c.issueID, service.RunBudgetKindExhausted).Scan(&content)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestRunBudgetExhaustionNoticeListsLastThreeRunResults(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310020, mergeEdgeSteps, 4)
	c.spend(t, 4)
	// Oldest first; the notice must show only the newest three, newest first.
	for i, out := range []string{"alpha oldest", "bravo", "charlie", "delta newest"} {
		if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET result=jsonb_build_object('output', $2::text), created_at=now()-($3::int * interval '1 minute')
			WHERE id = (SELECT id FROM agent_task_queue WHERE issue_id=$1 ORDER BY created_at LIMIT 1 OFFSET $4)`,
			c.issueID, out, 10-i, i); err != nil {
			t.Fatal(err)
		}
	}
	c.refuse(t)
	c.waitNotices(t, service.RunBudgetKindExhausted, 1)
	got := c.exhaustedNotice(t)
	for _, want := range []string{"Last 3 runs", "delta newest", "charlie", "bravo"} {
		if !strings.Contains(got, want) {
			t.Fatalf("notice lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "alpha oldest") {
		t.Fatalf("notice shows a fourth run:\n%s", got)
	}
	if strings.Index(got, "delta newest") > strings.Index(got, "bravo") {
		t.Fatalf("runs are not newest first:\n%s", got)
	}
}

func TestRunBudgetExhaustionNoticeShowsFailedRunError(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310021, mergeEdgeSteps, 2)
	c.spend(t, 2)
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='failed', error='runtime offline' WHERE id = (SELECT id FROM agent_task_queue WHERE issue_id=$1 ORDER BY created_at DESC LIMIT 1)`, c.issueID); err != nil {
		t.Fatal(err)
	}
	c.refuse(t)
	c.waitNotices(t, service.RunBudgetKindExhausted, 1)
	if got := c.exhaustedNotice(t); !strings.Contains(got, "failed") || !strings.Contains(got, "runtime offline") {
		t.Fatalf("notice lacks the failed run and its error:\n%s", got)
	}
}
