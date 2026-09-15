package handler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// TestChildDoneAgentEnqueueRaceCoalescesWithoutLeaking is the CHE-526
// admission-boundary regression for triggerChildDoneAgent. Before this
// change, its EnqueueTaskForMention branch logged every enqueue failure
// through a bare slog.Warn, unlike every other admission boundary already
// routed through logCommentEnqueueFailure (C1 issue_trigger.go, C2
// comment.go, C3 task_lifecycle.go). HasPendingTaskForIssueAndAgent guards
// the common case, but concurrent dispatch of the SAME wake (two children of
// one batch closing a stage back-to-back) can both pass that check before
// either commits its insert — the loser must coalesce at debug level, not
// surface a raw constraint-name warning.
//
// Mirrors the C1 concurrency pattern (TestEnqueueTaskForIssueConcurrentAssignAdmitsOnce):
// many goroutines call triggerChildDoneAgent for the SAME (parent, trigger
// comment) simultaneously so the check-then-act race is exercised for real
// rather than assumed.
func TestChildDoneAgentEnqueueRaceCoalescesWithoutLeaking(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newChildDoneFixture(t, "in_progress")
	agentID := createHandlerTestAgent(t, "CHE-526 child-done agent race", nil)
	setIssueAssigneeDirect(t, fx.parent.ID, "agent", agentID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, fx.parent.ID)
	})

	ctx := context.Background()
	parent, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(fx.parent.ID))
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	triggerCommentID := insertDupRaceComment(t, fx.parent.ID, "child-done wake", "1 minute")
	triggerUUID := util.MustParseUUID(triggerCommentID)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const concurrency = 25
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testHandler.triggerChildDoneAgent(ctx, parent, triggerUUID)
		}()
	}
	wg.Wait()

	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 1 {
		t.Fatalf("after %d concurrent dispatches: pending tasks = %d, want exactly 1", concurrency, got)
	}
	for _, leak := range []string{"idx_one_pending_task_per_issue_agent", "level=WARN", "level=ERROR"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("benign child-done agent enqueue race leaked %q into logs:\n%s", leak, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "duplicate pending task") {
		t.Fatalf("expected at least one debug-level coalesce log among %d concurrent dispatches, got:\n%s", concurrency, logs.String())
	}
}

// TestChildDoneSquadEnqueueRaceCoalescesWithoutLeaking mirrors
// TestChildDoneAgentEnqueueRaceCoalescesWithoutLeaking for the squad-leader
// path (triggerChildDoneSquad / EnqueueTaskForSquadLeader), which shared the
// same bare slog.Warn defect before this change.
func TestChildDoneSquadEnqueueRaceCoalescesWithoutLeaking(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newChildDoneFixture(t, "in_progress")
	sq := newSquadCommentTriggerFixture(t)
	setIssueAssigneeDirect(t, fx.parent.ID, "squad", sq.SquadID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, fx.parent.ID)
	})

	ctx := context.Background()
	parent, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(fx.parent.ID))
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	triggerCommentID := insertDupRaceComment(t, fx.parent.ID, "child-done wake", "1 minute")
	triggerUUID := util.MustParseUUID(triggerCommentID)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const concurrency = 25
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testHandler.triggerChildDoneSquad(ctx, parent, triggerUUID)
		}()
	}
	wg.Wait()

	if got := countPendingTasksForAgent(t, fx.parent.ID, sq.LeaderID); got != 1 {
		t.Fatalf("after %d concurrent dispatches: pending tasks = %d, want exactly 1", concurrency, got)
	}
	for _, leak := range []string{"idx_one_pending_task_per_issue_agent", "level=WARN", "level=ERROR"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("benign child-done squad enqueue race leaked %q into logs:\n%s", leak, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "duplicate pending task") {
		t.Fatalf("expected at least one debug-level coalesce log among %d concurrent dispatches, got:\n%s", concurrency, logs.String())
	}
}

// TestChildDoneAdmissionIdentityDistinctParents proves the child-done wake
// admits exactly the executing agent named by EACH parent's own assignment,
// independent of the duplicate/race handling above — the identity half of
// the admission boundary. Two unrelated parent/child pairs, each with its
// own agent assignee, must not cross-pollinate pending tasks.
func TestChildDoneAdmissionIdentityDistinctParents(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fxA := newChildDoneFixture(t, "in_progress")
	fxB := newChildDoneFixture(t, "in_progress")
	agentA := createHandlerTestAgent(t, "CHE-526 admission identity A", nil)
	agentB := createHandlerTestAgent(t, "CHE-526 admission identity B", nil)
	setIssueAssigneeDirect(t, fxA.parent.ID, "agent", agentA)
	setIssueAssigneeDirect(t, fxB.parent.ID, "agent", agentB)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id IN ($1, $2)`, fxA.parent.ID, fxB.parent.ID)
	})

	updateChildStatus(t, fxA.child.ID, "done")

	if got := countPendingTasksForAgent(t, fxA.parent.ID, agentA); got != 1 {
		t.Fatalf("parent A pending count = %d, want 1", got)
	}
	if got := countPendingTasksForAgent(t, fxB.parent.ID, agentB); got != 0 {
		t.Fatalf("unrelated parent B pending count = %d, want 0 — child-done wake must not cross parents", got)
	}
}

// TestChildDoneRepeatedCompletionOnlyWakesOnce proves a child that is saved
// as "done" more than once (the idempotent no-op transition already covered
// by TestChildDoneNotificationIsIdempotent for the comment side) does not
// accumulate extra pending tasks on the parent either — the wake dispatch
// side of the same guarantee.
func TestChildDoneRepeatedCompletionOnlyWakesOnce(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newChildDoneFixture(t, "in_progress")
	agentID := createHandlerTestAgent(t, "CHE-526 repeated completion", nil)
	setIssueAssigneeDirect(t, fx.parent.ID, "agent", agentID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, fx.parent.ID)
	})

	updateChildStatus(t, fx.child.ID, "done")
	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 1 {
		t.Fatalf("after first done: pending tasks = %d, want 1", got)
	}

	// Re-saving the already-terminal status is not a transition at all
	// (notifyParentOfChildDone returns before any wake dispatch), so no
	// second task should appear.
	updateChildStatus(t, fx.child.ID, "done")
	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 1 {
		t.Fatalf("after second (repeated) done: pending tasks = %d, want still 1", got)
	}
}

// TestChildDoneStageGatePreservedAcrossOpenLaterStage proves the stage
// barrier still gates the wake dispatch after the CHE-526 routing change:
// completing an early-stage child while a LATER stage still has open
// siblings must not fire the parent-assignee wake at all — the barrier
// belongs to notifyParentOfChildDone, upstream of dispatchParentAssigneeTrigger,
// and this change must not have widened it.
func TestChildDoneStageGatePreservedAcrossOpenLaterStage(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newChildDoneFixture(t, "in_progress")
	agentID := createHandlerTestAgent(t, "CHE-526 stage gate", nil)
	setIssueAssigneeDirect(t, fx.parent.ID, "agent", agentID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, fx.parent.ID)
	})

	// fx.child becomes stage 1; add a stage 2 sibling that stays open.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET stage = 1 WHERE id = $1`, fx.child.ID,
	); err != nil {
		t.Fatalf("stage child: %v", err)
	}
	var laterStageChildID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, status, priority, number, position, parent_issue_id, stage)
		VALUES ($1, 'member', $2, 'CHE-526 later stage sibling', 'in_progress', 'none',
		        (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1), 0, $3, 2)
		RETURNING id
	`, testWorkspaceID, testUserID, fx.parent.ID).Scan(&laterStageChildID); err != nil {
		t.Fatalf("create later-stage sibling: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, laterStageChildID) })

	updateChildStatus(t, fx.child.ID, "done")

	if got := countSystemCommentsOn(t, fx.parent.ID); got != 1 {
		t.Fatalf("stage 1 closing (only stage) should still fire once, got %d comments", got)
	}
	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 1 {
		t.Fatalf("stage 1 close should wake the parent once, got %d pending tasks", got)
	}

	// Completing the later-stage sibling closes stage 2 too — a second,
	// legitimate wake, not a leak from the first.
	updateChildStatus(t, laterStageChildID, "done")
	if got := countSystemCommentsOn(t, fx.parent.ID); got != 2 {
		t.Fatalf("stage 2 closing should add a second comment, got %d", got)
	}
}
