package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2886 (CHE-2297 finding 2): the raise the snapshot and the exhaustion
// notice show leaves exactly the balance they promise, read through admission.

// applyShownRaise runs the raise the snapshot shows and returns what it promised.
func applyShownRaise(t *testing.T, c edgeCase) service.RunBudgetPreview {
	t.Helper()
	snap := visibleSnapshot(t, c)
	if snap.RaisePreview == nil {
		t.Fatalf("snapshot shows no raise: %+v", snap)
	}
	p := *snap.RaisePreview
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": p.LimitAfter}, nil); w.Code != http.StatusOK {
		t.Fatalf("raise to %d = %d: %s", p.LimitAfter, w.Code, w.Body.String())
	}
	return p
}

func wantAdmitted(t *testing.T, c edgeCase, want int) {
	t.Helper()
	if got := admitUntilRefused(t, c, want+20); got != want {
		t.Fatalf("runs admitted after the shown raise = %d, want %d (the promised balance_after)", got, want)
	}
}

func TestShownRaiseLeavesThePromisedBalanceAfterAProgressGrant(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 40)
	if _, err := service.OpenRunBudgetWindow(context.Background(), testHandler.Queries, parseUUID(c.issueID)); err != nil {
		t.Fatal(err)
	}
	grantHead(t, c.issueID, "aaa") // 6 runs left
	p := applyShownRaise(t, c)
	if p.BalanceAfter != 16 {
		t.Fatalf("promised balance = %d, want 16 (6 left + 10)", p.BalanceAfter)
	}
	wantAdmitted(t, c, p.BalanceAfter)
}

func TestShownRaiseLeavesThePromisedBalanceAfterARefund(t *testing.T) {
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
	cancelled, err := testHandler.TaskService.CancelTaskWithReason(ctx, task.ID, "cancelled", service.FailureReasonSupersededHead)
	if err != nil {
		t.Fatal(err)
	}
	if err := inGrantTx(t, func(q *db.Queries) error { return service.RefundSupersededRun(ctx, q, *cancelled) }); err != nil {
		t.Fatal(err)
	}
	p := applyShownRaise(t, c) // 1 run left
	if p.BalanceAfter != 11 {
		t.Fatalf("promised balance = %d, want 11 (1 left + 10)", p.BalanceAfter)
	}
	wantAdmitted(t, c, p.BalanceAfter)
}

// carryDebt overdraws a cap-2 issue by n runs with member mentions, then lets a
// member top it up: the new window holds no tasks and the balance is n-2 below zero.
func carryDebt(t *testing.T, n int) edgeCase {
	t.Helper()
	ctx := context.Background()
	c, trigger := atZero(t)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		comment := dbfx.Comment(t, c.issueID, "@agent please look", testutil.Cols{})
		if _, err := testHandler.enqueueSingleCommentTrigger(ctx, issue, parseUUID(comment), trigger); err != nil {
			t.Fatalf("member mention %d at zero refused: %v", i+1, err)
		}
		finishRuns(t, c)
	}
	settle()
	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	settle()
	return c
}

func TestShownRaiseLeavesThePromisedBalanceUnderCarriedDebt(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c := carryDebt(t, 5) // balance -5, +2 from the top-up = -3, no tasks in the window
	if got := ledgerBalance(t, c.issueID); got != -3 {
		t.Fatalf("balance after the top-up = %d, want -3", got)
	}
	if n := admitUntilRefused(t, c, 1); n != 0 {
		t.Fatal("a run was admitted under debt")
	}
	p := applyShownRaise(t, c)
	if p.BalanceAfter != 10 || p.LimitAfter != 15 {
		t.Fatalf("preview = %+v, want 10 runs left at a cap of 15 (cap 2 + 3 debt + 10)", p)
	}
	wantAdmitted(t, c, p.BalanceAfter)
}

func TestExhaustionNoticeShowsTheSnapshotRaiseAndBalance(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c := carryDebt(t, 5)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run under debt = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	p := visibleSnapshot(t, c).RaisePreview
	if p == nil {
		t.Fatal("snapshot shows no raise")
	}
	cmd := fmt.Sprintf("--runs %d`", p.LimitAfter)
	left := fmt.Sprintf("leaves %d runs", p.BalanceAfter)
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted' AND details->>'raise_command' LIKE '% --runs '||$2::text`, c.issueID, fmt.Sprint(p.LimitAfter)); n != 1 {
		t.Fatalf("exhaustion activity carrying the snapshot's raise command (%s) = %d, want 1", cmd, n)
	}
	var content string
	if err := testPool.QueryRow(ctx, `SELECT content FROM comment WHERE issue_id=$1 AND type='system' AND content LIKE '%budget raise%' ORDER BY created_at DESC LIMIT 1`, c.issueID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, cmd) || !strings.Contains(content, left) {
		t.Fatalf("notice %q lacks the snapshot's command (%s) or balance (%s)", content, cmd, left)
	}
}

// noticeRaise returns the raise command the exhaustion activity recorded and
// the notice text, so a test can hold them against the snapshot.
func noticeRaise(t *testing.T, c edgeCase) (command, notice string) {
	t.Helper()
	ctx := context.Background()
	if err := testPool.QueryRow(ctx, `SELECT details->>'raise_command' FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted' ORDER BY created_at DESC LIMIT 1`, c.issueID).Scan(&command); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT content FROM comment WHERE issue_id=$1 AND type='system' AND content LIKE '%budget raise%' ORDER BY created_at DESC LIMIT 1`, c.issueID).Scan(&notice); err != nil {
		t.Fatal(err)
	}
	return command, notice
}

// wantNoticeMatchesSnapshot holds the activity's command and the notice's
// command and balance against the snapshot's raise preview.
func wantNoticeMatchesSnapshot(t *testing.T, c edgeCase) service.RunBudgetPreview {
	t.Helper()
	p := visibleSnapshot(t, c).RaisePreview
	if p == nil {
		t.Fatal("snapshot shows no raise")
	}
	command, notice := noticeRaise(t, c)
	if want := fmt.Sprintf(" --runs %d", p.LimitAfter); !strings.HasSuffix(command, want) {
		t.Fatalf("activity raise command %q, want one ending %q (the snapshot's)", command, want)
	}
	if want, left := fmt.Sprintf("--runs %d`", p.LimitAfter), fmt.Sprintf("leaves %d runs", p.BalanceAfter); !strings.Contains(notice, want) || !strings.Contains(notice, left) {
		t.Fatalf("notice %q lacks the snapshot's command (%s) or balance (%s)", notice, want, left)
	}
	return *p
}

// The blocked step's handler gets one recovery run, which debits a run after
// the notice is written. The command and the balance the notice shows must
// already count it.
func TestExhaustionNoticeCountsTheRecoveryRunItGives(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c := newBudgetCase(t, 2886101, recoverySteps(seededReadyAgentID(t)), 2)
	e := refusedEvent(t, c)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if n := recoveryRuns(t, c.edgeCase); n != 1 {
		t.Fatalf("recovery runs = %d, want 1", n)
	}
	finishRuns(t, c.edgeCase)
	c.enter(t, "in_progress")
	p := wantNoticeMatchesSnapshot(t, c.edgeCase)
	if p.BalanceAfter != 10 {
		t.Fatalf("promised balance = %d, want 10", p.BalanceAfter)
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": p.LimitAfter}, nil); w.Code != http.StatusOK {
		t.Fatalf("raise to %d = %d: %s", p.LimitAfter, w.Code, w.Body.String())
	}
	wantAdmitted(t, c.edgeCase, p.BalanceAfter)
}

// namedRun admits one member-mentioned run past zero, which debits the ledger.
func namedRun(t *testing.T, c edgeCase) {
	t.Helper()
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "raise-named-"+uuidTail(), nil)
	agent, err := testHandler.Queries.GetAgent(ctx, parseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	comment := dbfx.Comment(t, c.issueID, "@agent please look", testutil.Cols{})
	if _, err := testHandler.enqueueSingleCommentTrigger(ctx, issue, parseUUID(comment), commentAgentTrigger{Agent: agent, Source: commentTriggerSourceMentionAgent}); err != nil {
		t.Fatalf("member mention at zero refused: %v", err)
	}
	finishRuns(t, c)
}

// A person's named run can debit the ledger between the refusal and the
// callback. The notice reads the balance the block sees, not the refusal's.
func TestExhaustionNoticeFollowsABalanceThatMovedAfterTheRefusal(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	for name, land := range map[string]func(*testing.T, edgeCase){
		"before the callback reads": func(t *testing.T, c edgeCase) { namedRun(t, c) },
		"inside the block's window": func(t *testing.T, c edgeCase) { landBeforeBlock(t, func() { namedRun(t, c) }) },
	} {
		t.Run(name, func(t *testing.T) {
			c := newBudgetCase(t, 2886102, mergeEdgeSteps, 2)
			e := refusedEvent(t, c)
			land(t, c.edgeCase)
			testHandler.onRunBudgetExhausted(context.Background(), e)
			if got := ledgerBalance(t, c.issueID); got != -1 {
				t.Fatalf("balance = %d, want -1 (the named run's debit)", got)
			}
			c.enter(t, "in_progress")
			p := wantNoticeMatchesSnapshot(t, c.edgeCase)
			if p.LimitAfter != 13 || p.BalanceAfter != 10 {
				t.Fatalf("preview = %+v, want a cap of 13 leaving 10 (cap 2 + 1 debt + 10)", p)
			}
			if w := raiseBudget(t, c.issueID, map[string]any{"runs": p.LimitAfter}, nil); w.Code != http.StatusOK {
				t.Fatalf("raise to %d = %d: %s", p.LimitAfter, w.Code, w.Body.String())
			}
			wantAdmitted(t, c.edgeCase, p.BalanceAfter)
		})
	}
}
