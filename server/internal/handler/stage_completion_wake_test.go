package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newStageWakeFixture creates a parent assigned to the ready test agent plus
// n staged (all stage 1) children, so a test can drive completions and assert
// on stage_completion_wake / stage_generation directly. Mirrors
// newStagedBatchFixture (issue_batch_test.go) but with a single stage and a
// caller-controlled child count, for the 20-child concurrency requirement in
// CHE-482 proposal 5's acceptance criteria.
type stageWakeFixture struct {
	parent   IssueResponse
	agentID  string
	children []IssueResponse
}

func newStageWakeFixture(t *testing.T, n int) stageWakeFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}

	pw := httptest.NewRecorder()
	preq := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "stage-wake parent " + time.Now().Format(time.RFC3339Nano),
		"status": "in_progress",
	})
	testHandler.CreateIssue(pw, preq)
	if pw.Code != http.StatusCreated {
		t.Fatalf("create parent: expected 201, got %d: %s", pw.Code, pw.Body.String())
	}
	var parent IssueResponse
	if err := json.NewDecoder(pw.Body).Decode(&parent); err != nil {
		t.Fatalf("decode parent: %v", err)
	}

	var agentID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id FROM agent WHERE workspace_id = $1 AND name = $2`,
		testWorkspaceID, "Handler Test Agent",
	).Scan(&agentID); err != nil {
		t.Fatalf("locate test agent: %v", err)
	}
	setIssueAssigneeDirect(t, parent.ID, "agent", agentID)

	children := make([]IssueResponse, 0, n)
	for i := 0; i < n; i++ {
		cw := httptest.NewRecorder()
		creq := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
			"title":           "stage-wake child " + time.Now().Format(time.RFC3339Nano),
			"status":          "in_progress",
			"parent_issue_id": parent.ID,
		})
		testHandler.CreateIssue(cw, creq)
		if cw.Code != http.StatusCreated {
			t.Fatalf("create child: expected 201, got %d: %s", cw.Code, cw.Body.String())
		}
		var child IssueResponse
		if err := json.NewDecoder(cw.Body).Decode(&child); err != nil {
			t.Fatalf("decode child: %v", err)
		}
		if _, err := testPool.Exec(context.Background(),
			`UPDATE issue SET stage = 1 WHERE id = $1`, child.ID); err != nil {
			t.Fatalf("set child stage: %v", err)
		}
		children = append(children, child)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, parent.ID)
		testPool.Exec(ctx, `DELETE FROM stage_completion_wake WHERE parent_issue_id = $1`, parent.ID)
		testPool.Exec(ctx, `DELETE FROM stage_generation WHERE parent_issue_id = $1`, parent.ID)
		for _, c := range children {
			testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, c.ID)
		}
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, parent.ID)
	})

	return stageWakeFixture{parent: parent, agentID: agentID, children: children}
}

func countStageCompletionWakes(t *testing.T, parentID string, stage int32) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM stage_completion_wake WHERE parent_issue_id = $1 AND stage = $2`,
		parentID, stage,
	).Scan(&n); err != nil {
		t.Fatalf("count stage_completion_wake: %v", err)
	}
	return n
}

func currentStageGenerationDB(t *testing.T, parentID string, stage int32) int32 {
	t.Helper()
	var gen int32
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT generation FROM stage_generation WHERE parent_issue_id = $1 AND stage = $2), 0)`,
		parentID, stage,
	).Scan(&gen); err != nil {
		t.Fatalf("read stage_generation: %v", err)
	}
	return gen
}

// TestStageWake_TwentyChildConcurrentCloses_ExactlyOneWake is the CHE-482
// proposal 5 acceptance criterion, directly: "Duplicate, concurrent, and
// out-of-order completions for a 20-child stage admit exactly one completion
// wake per satisfied generation." 19 children are already done; 20 goroutines
// race to close the last one (some duplicating the same final UpdateIssue
// call), reproducing the double-wake shape from production (CHE-488: two
// system comments 15h44m apart for the same parent/stage/child) but
// compressed into genuine concurrency instead of a long delay.
func TestStageWake_TwentyChildConcurrentCloses_ExactlyOneWake(t *testing.T) {
	fx := newStageWakeFixture(t, 20)
	for _, c := range fx.children[:19] {
		updateChildStatus(t, c.ID, "done")
	}
	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 0 {
		t.Fatalf("wake claimed before the stage actually closed: %d rows", got)
	}

	last := fx.children[19]
	const racers = 20
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			// Every racer targets the SAME transition (in_progress -> done) on
			// the SAME child — the duplicate/concurrent completion case, not
			// N different children.
			w := httptest.NewRecorder()
			req := newRequest("PUT", "/api/issues/"+last.ID, map[string]any{"status": "done"})
			req = withURLParam(req, "id", last.ID)
			testHandler.UpdateIssue(w, req)
		}()
	}
	wg.Wait()

	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 1 {
		t.Fatalf("expected exactly 1 stage_completion_wake row after 20 concurrent closes, got %d", got)
	}
	if got := countSystemCommentsOn(t, fx.parent.ID); got != 1 {
		t.Fatalf("expected exactly 1 system comment, got %d — this is the CHE-488 production bug shape (duplicate wake)", got)
	}
	if got := countPendingTasksForAgent(t, fx.parent.ID, fx.agentID); got != 1 {
		t.Fatalf("expected exactly 1 pending parent task, got %d", got)
	}
}

// TestStageWake_Reopen_InvalidatesGeneration_ExactlyOneWakePerGeneration is
// the CHE-488 core scenario reconstructed exactly: a staged child reaches
// done (stage closes, generation 0 wakes), review finds a defect and the
// child is reopened (generation bumps to 1, the recorded generation-0 wake
// stays but can never fire again), then the correction lands and the child
// reaches done again (generation 1 closes and wakes — this is the wanted
// "wake once more after a real correction", not a duplicate). Total: exactly
// 2 wake rows, one per generation, matching CHE-482: "Reopened stages ...
// never produce unauthorized advancement" (the reopen itself doesn't wake
// anything) while still allowing the legitimate re-closure to notify.
func TestStageWake_Reopen_InvalidatesGeneration_ExactlyOneWakePerGeneration(t *testing.T) {
	fx := newStageWakeFixture(t, 2)
	updateChildStatus(t, fx.children[0].ID, "done")
	updateChildStatus(t, fx.children[1].ID, "done")

	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 1 {
		t.Fatalf("expected 1 wake after first close, got %d", got)
	}
	if got := currentStageGenerationDB(t, fx.parent.ID, 1); got != 0 {
		t.Fatalf("expected generation 0 before any reopen, got %d", got)
	}

	// Reopen: PR #33 shape from CHE-485 — a merged/done child found to have a
	// defect, reopened for correction.
	updateChildStatus(t, fx.children[1].ID, "in_progress")
	if got := currentStageGenerationDB(t, fx.parent.ID, 1); got != 1 {
		t.Fatalf("expected generation to bump to 1 after reopen, got %d", got)
	}
	// The reopen itself must not add a second wake for the now-invalidated
	// generation 0 barrier (the stage is no longer closed: child 1 is back to
	// in_progress) — CHE-482: reopened stages must not produce unauthorized
	// advancement.
	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 1 {
		t.Fatalf("reopen must not itself claim a wake, still expected 1, got %d", got)
	}
	if got := countSystemCommentsOn(t, fx.parent.ID); got != 1 {
		t.Fatalf("reopen must not itself post a comment, still expected 1, got %d", got)
	}

	// The real correction lands and the stage re-closes.
	updateChildStatus(t, fx.children[1].ID, "done")

	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 2 {
		t.Fatalf("expected 2 total wake rows (one per generation) after the legitimate re-closure, got %d", got)
	}
	if got := countSystemCommentsOn(t, fx.parent.ID); got != 2 {
		t.Fatalf("expected 2 system comments (one per generation), got %d", got)
	}

	var generations []int32
	rows, err := testPool.Query(context.Background(),
		`SELECT generation FROM stage_completion_wake WHERE parent_issue_id = $1 AND stage = 1 ORDER BY generation`,
		fx.parent.ID)
	if err != nil {
		t.Fatalf("list wake generations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g int32
		if err := rows.Scan(&g); err != nil {
			t.Fatalf("scan generation: %v", err)
		}
		generations = append(generations, g)
	}
	if len(generations) != 2 || generations[0] != 0 || generations[1] != 1 {
		t.Fatalf("expected wake generations [0 1], got %v", generations)
	}
}

// TestStageWake_ReplayAfterCrash_ZeroAdditionalWakes simulates the exact
// production defect: notifyParentOfChildDone (or the batch path) is invoked
// a SECOND time for the identical already-committed transition — the shape
// of a crash/restart replay where the caller does not know whether the first
// attempt's wake was already recorded. Unlike HasPendingTaskForIssueAndAgent
// (which only dedupes while the earlier task is still pending, see
// issue_child_done.go), stage_completion_wake is a durable row: this test
// drains the pending task first (simulating that the earlier task has long
// since finished, exactly like CHE-488's 15h44m production gap) before
// replaying, so only the durable table — not the pending-task guard — can
// catch the duplicate.
func TestStageWake_ReplayAfterCrash_ZeroAdditionalWakes(t *testing.T) {
	fx := newStageWakeFixture(t, 2)
	ctx := context.Background()

	updateChildStatus(t, fx.children[0].ID, "done")
	// Drive the real transition through the HTTP path first, exactly like the
	// production case: the status update (and its own notifyParentOfChildDone
	// call) has already committed by the time anything replays.
	updateChildStatus(t, fx.children[1].ID, "done")
	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 1 {
		t.Fatalf("expected 1 wake after the real close, got %d", got)
	}

	// Simulate the earlier task having long since finished (CHE-488's
	// production gap was 15h44m) so HasPendingTaskForIssueAndAgent cannot
	// mask the replay — the durable table must be what catches it.
	if _, err := testPool.Exec(ctx,
		`UPDATE agent_task_queue SET status = 'completed' WHERE issue_id = $1`, fx.parent.ID,
	); err != nil {
		t.Fatalf("mark parent task completed: %v", err)
	}

	// Replay: notifyParentOfChildDone invoked again for the IDENTICAL already-
	// committed transition — the shape of a crash/restart retry where the
	// caller does not know whether the first attempt's wake was recorded.
	// ListChildIssues re-reads fresh from DB inside claimStageCompletionWake,
	// so this exercises the real code path, not a hand-built snapshot.
	child1, err := testHandler.Queries.GetIssue(ctx, parseUUID(fx.children[1].ID))
	if err != nil {
		t.Fatalf("load child1: %v", err)
	}
	prev1 := child1
	prev1.Status = "in_progress"
	testHandler.notifyParentOfChildDone(ctx, prev1, child1)

	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 1 {
		t.Fatalf("replay after crash must add zero wakes, still expected 1, got %d", got)
	}
	if got := countSystemCommentsOn(t, fx.parent.ID); got != 1 {
		t.Fatalf("replay after crash must add zero comments, still expected 1, got %d", got)
	}
}

// TestStageWake_CancelledChild_DistinctFromDoneButStillWakesOnce covers
// CHE-482's "cancellation ... must not imply successful acceptance" at the
// wake-identity layer: a stage closed by a CANCELLED last child still gets
// exactly one durable wake row (the coordinator IS told the stage is no
// longer open — a cancelled sibling never finishes, so it can't hold the
// barrier — see stageBarrierClosed), and the generated comment is
// distinguishable (it names the cancelled child, not a synthesized "done"
// claim), never inflating into two wakes the way a done-vs-cancelled status
// confusion might.
func TestStageWake_CancelledChild_DistinctFromDoneButStillWakesOnce(t *testing.T) {
	fx := newStageWakeFixture(t, 2)
	updateChildStatus(t, fx.children[0].ID, "done")
	updateChildStatus(t, fx.children[1].ID, "cancelled")

	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 1 {
		t.Fatalf("expected exactly 1 wake when the stage closes via cancellation, got %d", got)
	}
	content := parentSystemCommentContent(t, fx.parent.ID)
	if !strings.Contains(content, fx.children[1].Identifier) {
		t.Errorf("comment should name the cancelled child that closed the barrier, got: %s", content)
	}
}

// TestStageWake_ConcurrentDistinctStages_EachGetsExactlyOneWake guards
// against a lock or generation key that accidentally collapses different
// stages of the same parent into one bucket: stage 1 and stage 2 wakes must
// be independently keyed (different `stage` values in stage_completion_wake),
// not a single winner-take-all claim across the whole parent. Stage 1 is
// closed first and deterministically (closing stage 2 requires stage 1
// already terminal — stageBarrierClosed's frontier-closure rule — so racing
// them against each other would just be testing that ordering constraint,
// not this concern); stage 2's own closure is then raced to reconfirm the
// per-generation claim still holds once a SECOND stage exists on the same
// parent.
func TestStageWake_ConcurrentDistinctStages_EachGetsExactlyOneWake(t *testing.T) {
	fx := newStageWakeFixture(t, 4)
	// Split into two stages of 2 children each.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET stage = 2 WHERE id = ANY($1)`,
		[]string{fx.children[2].ID, fx.children[3].ID},
	); err != nil {
		t.Fatalf("set stage 2: %v", err)
	}
	updateChildStatus(t, fx.children[0].ID, "done")
	updateChildStatus(t, fx.children[1].ID, "done")
	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 1 {
		t.Fatalf("expected exactly 1 wake for stage 1, got %d", got)
	}

	updateChildStatus(t, fx.children[2].ID, "done")

	const racers = 10
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			req := newRequest("PUT", "/api/issues/"+fx.children[3].ID, map[string]any{"status": "done"})
			req = withURLParam(req, "id", fx.children[3].ID)
			testHandler.UpdateIssue(w, req)
		}()
	}
	wg.Wait()

	if got := countStageCompletionWakes(t, fx.parent.ID, 1); got != 1 {
		t.Errorf("expected exactly 1 wake for stage 1 (unaffected by stage 2's race), got %d", got)
	}
	if got := countStageCompletionWakes(t, fx.parent.ID, 2); got != 1 {
		t.Errorf("expected exactly 1 wake for stage 2, got %d", got)
	}
}
