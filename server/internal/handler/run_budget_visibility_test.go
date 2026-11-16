package handler

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2297 finding 2: the snapshot, the run prompt and the 80% threshold read
// the balance admission reads, not the task count.

func visibleSnapshot(t *testing.T, c edgeCase) service.RunBudgetSnapshot {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := service.LoadRunBudgetSnapshot(context.Background(), testHandler.Queries, issue)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func visibleBrief(t *testing.T, c edgeCase) string {
	t.Helper()
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	got := testHandler.claimProjectWorkflow(ctx, issue, &db.AgentTaskQueue{})
	if got == nil {
		t.Fatal("claim carries no workflow")
	}
	return got.Brief
}

// wantVisible checks the snapshot and the prompt agree with the admitted balance.
func wantVisible(t *testing.T, c edgeCase, left int64, promptLine string) service.RunBudgetSnapshot {
	t.Helper()
	snap := visibleSnapshot(t, c)
	if snap.RunsLeft != left || slices.Contains(snap.Blocking, "runs") {
		t.Fatalf("snapshot runs left = %d, blocking = %v, want %d and no runs", snap.RunsLeft, snap.Blocking, left)
	}
	if brief := visibleBrief(t, c); !strings.Contains(brief, promptLine) {
		t.Fatalf("prompt is missing %q:\n%s", promptLine, brief)
	}
	return snap
}

func TestRunBudgetVisibilityShowsAProgressGrantOnAnExhaustedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 40)
	if _, err := service.OpenRunBudgetWindow(context.Background(), testHandler.Queries, parseUUID(c.issueID)); err != nil {
		t.Fatal(err)
	}
	if snap := visibleSnapshot(t, c); snap.RunsLeft != 0 || !slices.Contains(snap.Blocking, "runs") {
		t.Fatalf("exhausted window: runs left = %d, blocking = %v, want 0 and runs", snap.RunsLeft, snap.Blocking)
	}
	grantHead(t, c.issueID, "aaa")
	wantVisible(t, c, service.RunBudgetGrantRuns, "Run budget: 6 runs left of 40;")
}

func TestRunBudgetVisibilityShowsASupersededHeadRefund(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, _ := budgetedAt(t, 40, 39)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if snap := visibleSnapshot(t, c); !slices.Contains(snap.Blocking, "runs") {
		t.Fatalf("40 of 40 run: blocking = %v, want runs", snap.Blocking)
	}
	cancelled, err := testHandler.TaskService.CancelTaskWithReason(ctx, task.ID, "cancelled", service.FailureReasonSupersededHead)
	if err != nil {
		t.Fatal(err)
	}
	if err := inGrantTx(t, func(q *db.Queries) error { return service.RefundSupersededRun(ctx, q, *cancelled) }); err != nil {
		t.Fatal(err)
	}
	wantVisible(t, c, 1, "Run budget: 1 runs left of 40;")
}

func TestRunBudgetVisibilityShowsARaisedIssueCap(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 5, 1)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET max_runs_override=20 WHERE id=$1`, c.issueID); err != nil {
		t.Fatal(err)
	}
	wantVisible(t, c, 19, "Run budget: 19 runs left of 20;")
}

func TestRunBudgetVisibilityNearUsesTheBalance(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 40)
	if _, err := service.OpenRunBudgetWindow(context.Background(), testHandler.Queries, parseUUID(c.issueID)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		grantHead(t, c.issueID, string(rune('a'+i)))
	}
	if snap := visibleSnapshot(t, c); snap.RunsLeft != 18 || snap.RunsState != service.RunBudgetOK {
		t.Fatalf("18 runs left of 40: left = %d, state = %q, want ok", snap.RunsLeft, snap.RunsState)
	}
}

// With a grant on the window the 4th of 5 runs leaves 7: no warning. The
// warning comes when the balance falls to a fifth of the cap.
func TestRunBudgetWarningFollowsTheBalance(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c := newBudgetCase(t, 2806001, mergeEdgeSteps, 5)
	c.spend(t, 3)
	grantHead(t, c.issueID, "aaa")
	c.spend(t, 1)
	if comments, _ := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 0 {
		t.Fatalf("warnings with 7 runs left of 5 = %d, want 0", comments)
	}
	c.spend(t, 6)
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 1 || inbox != 1 {
		t.Fatalf("warnings with 1 run left = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}
