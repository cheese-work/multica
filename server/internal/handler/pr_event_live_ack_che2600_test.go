package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2600: a typed live notice that the run accepts handles the cause. A legacy
// PR rule receipt, new or already waiting, must not start another run once the
// live run ends; a failed or unacknowledged notice keeps the one fallback wake.
// Every step runs through production entry points: task enqueue, claim and start
// with capability negotiation, the claim and ack handlers, and task completion.

type liveRun struct {
	fx     coalesceFixture
	taskID string
}

// queueRun enqueues the agent's run on the fixture's issue the way an assignment
// does; it stays queued until start.
func queueRun(t *testing.T, fx coalesceFixture) liveRun {
	t.Helper()
	runtimeID := dbfx.Runtime(t, "che2600-claude", testutil.Cols{"provider": "claude"})
	dbfx.Exec(t, `UPDATE agent SET runtime_id=$2 WHERE id=$1`, fx.agentID, runtimeID)
	issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(fx.issueID))
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.TaskService.EnqueueTaskForIssue(t.Context(), issue)
	if err != nil {
		t.Fatalf("EnqueueTaskForIssue: %v", err)
	}
	return liveRun{fx: fx, taskID: uuidToString(task.ID)}
}

// start claims the queued run and starts it, negotiating typed PR-event delivery.
func (r liveRun) start(t *testing.T) {
	t.Helper()
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id=$1`, r.fx.agentID).Scan(&runtimeID)
	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(t.Context(), parseUUID(runtimeID))
	if err != nil || claimed == nil || uuidToString(claimed.ID) != r.taskID {
		t.Fatalf("ClaimTaskForRuntime = %v, %v; want task %s", claimed, err, r.taskID)
	}
	if _, err := testHandler.TaskService.StartTask(t.Context(), claimed.ID, service.TaskStartCapabilities{PREvent: true}); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_pr_event_capability WHERE task_id=$1`, r.taskID); n != 1 {
		t.Fatalf("PR-event capability rows = %d, want 1", n)
	}
}

// ack claims the pending typed notice and acknowledges it through the daemon handlers.
func (r liveRun) ack(t *testing.T, delivered bool) {
	t.Helper()
	var claim map[string]any
	testutil.Call(t, testHandler.ClaimTaskPREvent, withURLParam(newOwnerDaemonRequest(http.MethodPost, "/claim", map[string]any{}), "taskId", r.taskID)).
		Want(http.StatusOK).JSON(&claim)
	eventID, _ := claim["event_id"].(string)
	if eventID == "" {
		t.Fatalf("claim carried no event: %v", claim)
	}
	req := withURLParams(newOwnerDaemonRequest(http.MethodPost, "/ack", map[string]any{"delivered": delivered}), "taskId", r.taskID, "eventId", eventID)
	testutil.Call(t, testHandler.AckTaskPREvent, req).Want(http.StatusOK)
}

func (r liveRun) complete(t *testing.T) {
	t.Helper()
	if _, err := testHandler.TaskService.CompleteTask(t.Context(), parseUUID(r.taskID), []byte(`{}`), "", "", "", false, "", ""); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
}

func TestLiveNoticeLeavesNoRedundantRun(t *testing.T) {
	causes := []struct {
		name string
		snap func(head string) ghsnapshot.PRSnapshot
	}{
		{"conflict", func(h string) ghsnapshot.PRSnapshot { return conflictingSnapshot(h, "base-1") }},
		{"checks_failed", failingSnapshot},
	}
	// legacyFirst: the run is still queued when the snapshot lands, so the legacy
	// rule queues its receipt before any typed notice exists.
	scenarios := []struct {
		name        string
		legacyFirst bool
		finish      func(t *testing.T, run liveRun)
		wantRuns    int
	}{
		{"acked live notice", false, func(t *testing.T, run liveRun) { run.ack(t, true); run.complete(t) }, 0},
		{"acked live notice after a waiting legacy receipt", true, func(t *testing.T, run liveRun) { run.ack(t, true); run.complete(t) }, 0},
		{"delivery failed", false, func(t *testing.T, run liveRun) { run.ack(t, false); run.complete(t) }, 1},
		{"turn ended before ack", false, func(t *testing.T, run liveRun) { run.complete(t) }, 1},
	}
	n := int32(260000)
	for _, cause := range causes {
		for _, sc := range scenarios {
			n++
			t.Run(cause.name+"/"+sc.name, func(t *testing.T) {
				if testHandler == nil {
					t.Skip("database not available")
				}
				head := "che2600-" + cause.name[:4] + "-" + sc.name[:4] + sc.name[len(sc.name)-3:]
				fx := newCoalesceFixture(t, int64(n), n, head)
				run := queueRun(t, fx)
				fx.set(cause.snap(head))
				if sc.legacyFirst {
					fx.refresh()
					run.start(t)
				} else {
					run.start(t)
					fx.refresh()
				}
				if sc.legacyFirst {
					fx.refresh() // the same snapshot again: the live run now holds the channel
				}
				sc.finish(t, run)
				fx.sweep()
				if got := fx.runs(); got != sc.wantRuns {
					t.Fatalf("started %d follow-up runs, want %d", got, sc.wantRuns)
				}
			})
		}
	}
}
