package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6992 (plan CHE-6971, step 6): a PR item and a joined condition firing sit
// in the same running run. Each settles through its own item; only the CHE-2600
// step retires the legacy pr_checks_failed receipt, and it never touches the
// condition receipt. Every step runs through production entry points.

func setLiveJoinMode(t *testing.T, mode string) {
	t.Helper()
	var original []byte
	if err := testPool.QueryRow(context.Background(), `SELECT COALESCE(settings, '{}'::jsonb) FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, original)
	})
	if _, err := testPool.Exec(context.Background(),
		`UPDATE workspace SET settings=COALESCE(settings, '{}'::jsonb)||jsonb_build_object('wakeup_live_join', jsonb_build_object('mode', $2::text)) WHERE id=$1`,
		testWorkspaceID, mode); err != nil {
		t.Fatal(err)
	}
}

// startV2 is start with the daemon offering v2: typed PR events plus wakeup items.
func (r liveRun) startV2(t *testing.T) {
	t.Helper()
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id=$1`, r.fx.agentID).Scan(&runtimeID)
	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(t.Context(), parseUUID(runtimeID))
	if err != nil || claimed == nil || uuidToString(claimed.ID) != r.taskID {
		t.Fatalf("ClaimTaskForRuntime = %v, %v; want task %s", claimed, err, r.taskID)
	}
	if _, err := testHandler.TaskService.StartTask(t.Context(), claimed.ID, service.TaskStartCapabilities{PREvent: true, PREventV2: true}); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
}

// claimItem claims the next pending item of the run and returns its id.
func (r liveRun) claimItem(t *testing.T) string {
	t.Helper()
	var claim map[string]any
	testutil.Call(t, testHandler.ClaimTaskPREvent, withURLParam(newOwnerDaemonRequest(http.MethodPost, "/claim", map[string]any{}), "taskId", r.taskID)).
		Want(http.StatusOK).JSON(&claim)
	id, _ := claim["event_id"].(string)
	if id == "" {
		t.Fatalf("claim carried no item: %v", claim)
	}
	return id
}

func (r liveRun) ackItem(t *testing.T, eventID string) {
	t.Helper()
	req := withURLParams(newOwnerDaemonRequest(http.MethodPost, "/ack", map[string]any{"delivered": true}), "taskId", r.taskID, "eventId", eventID)
	testutil.Call(t, testHandler.AckTaskPREvent, req).Want(http.StatusOK)
}

func TestLiveJoinAndChecksFailedItemSettleThroughTheirOwnItems(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const head = "che6992-checks"
	fx := newCoalesceFixture(t, 699201, 699201, head)
	setLiveJoinMode(t, "block")
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() { testHandler.TaskService.PRSnapshotsEnabled = false })

	// The head fails while the run is still queued: the legacy rule queues its
	// receipt and the snapshot pipeline captures the typed event. The run then
	// starts offering v2, and the same snapshot lands again.
	run := queueRun(t, fx)
	fx.set(failingSnapshot(head))
	fx.refresh()
	run.startV2(t)
	fx.refresh()
	var source pgtype.UUID
	if err := source.Scan(run.taskID); err != nil {
		t.Fatal(err)
	}
	ruleID := createRule(t, fx.issueID, fx.agentID, source, service.WakeupInput{
		Instruction: "Review the finished checks", Kind: "event", Mode: "continuous",
		Condition: json.RawMessage(`{"type":"pull_request","event":"checks_finished"}`),
	})
	dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() WHERE id = $1`, ruleID)
	runWakeupTick(t)

	if got := wakeupRunsOf(t, ruleID); got != 0 {
		t.Fatalf("the joined firing started %d runs of its own, want 0", got)
	}
	kinds := func() map[string]int {
		out := map[string]int{}
		rows, err := testPool.Query(context.Background(), `SELECT item_kind, count(*) FROM task_pr_event_delivery WHERE task_id=$1 GROUP BY 1`, run.taskID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				t.Fatal(err)
			}
			out[k] = n
		}
		return out
	}
	if got := kinds(); got["wakeup"] != 1 || got["pr_event"] != 1 {
		t.Fatalf("items by kind = %v, want one wakeup and one pr_event", got)
	}
	pending := func(where string, args ...any) int {
		return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id
			WHERE w.issue_id=$1 AND r.processed_at IS NULL AND `+where, append([]any{fx.issueID}, args...)...)
	}
	legacy := func() int { return pending(`w.system_rule='pr_checks_failed'`) }
	condition := func() int { return pending(`w.id=$2`, ruleID) }
	if legacy() != 1 || condition() != 1 {
		t.Fatalf("before any ack: pending legacy=%d condition=%d, want 1 and 1", legacy(), condition())
	}

	// Ack the PR item first: the CHE-2600 step retires the legacy receipt only.
	first, second := run.claimItem(t), run.claimItem(t)
	kindOf := func(id string) string {
		var k string
		dbfx.QueryRow(t, `SELECT item_kind FROM task_pr_event_delivery WHERE event_id=$1`, id).Scan(&k)
		return k
	}
	prItem, wakeItem := first, second
	if kindOf(first) == "wakeup" {
		prItem, wakeItem = second, first
	}
	if kindOf(prItem) != "pr_event" || kindOf(wakeItem) != "wakeup" {
		t.Fatalf("claimed kinds %s and %s, want one pr_event and one wakeup", kindOf(first), kindOf(second))
	}
	run.ackItem(t, prItem)
	if legacy() != 0 {
		t.Fatalf("the acked PR item left %d legacy receipts pending, want 0", legacy())
	}
	if condition() != 1 {
		t.Fatalf("the acked PR item touched the condition receipt: pending = %d, want 1", condition())
	}

	// The wakeup item settles through its own ack and the host's completion.
	run.ackItem(t, wakeItem)
	if condition() != 1 {
		t.Fatalf("a delivered wakeup item consumed its receipt before the host ended: pending = %d, want 1", condition())
	}
	run.complete(t)
	runWakeupTick(t)
	if condition() != 0 {
		t.Fatalf("after the host completed, the condition receipt is still pending (%d)", condition())
	}
	if outcome, task := lastWakeupOutcome(t, fx.issueID); outcome != "joined_live" || task != run.taskID {
		t.Fatalf("wakeup outcome = %q task %q, want joined_live in host %s", outcome, task, run.taskID)
	}
	fx.sweep()
	if got := wakeupRunsOf(t, ruleID); got != 0 || fx.runs() != 0 {
		t.Fatalf("follow-up runs: condition=%d pr rules=%d, want none", got, fx.runs())
	}
}
