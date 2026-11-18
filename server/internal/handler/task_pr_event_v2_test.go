package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-6987 (plan CHE-6971, SA-1): the typed PR-event channel gains capability
// v2 (v1 plus wakeup items). PR items must behave exactly as under v1.

func startOffering(t *testing.T, offered []string) (taskID string, started AgentTaskResponse) {
	t.Helper()
	f := newSupplementFixture(t, "codex", "dispatched", false)
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, f.taskID)
	req := withURLParam(newRequest(http.MethodPost, "/start", map[string]any{"capabilities": offered}), "taskId", f.taskID)
	testutil.Call(t, testHandler.StartTask, req).Want(http.StatusOK).JSON(&started)
	return f.taskID, started
}

func storedPREventCapability(t *testing.T, taskID string) (rows int, capability string) {
	t.Helper()
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*), COALESCE(max(capability), '') FROM task_pr_event_capability WHERE task_id = $1`, taskID,
	).Scan(&rows, &capability); err != nil {
		t.Fatal(err)
	}
	return rows, capability
}

func TestTaskPREventStartRecordsV2WhenOffered(t *testing.T) {
	v1, v2 := protocol.DaemonCapabilityTaskSupplementPREventV1, protocol.DaemonCapabilityTaskSupplementPREventV2
	for _, tc := range []struct {
		name     string
		offered  []string
		wantRows int
		want     string
	}{
		{"v1 and v2 offered records v2", []string{protocol.DaemonCapabilityTaskSupplementV1, v1, v2}, 1, v2},
		{"v2 offered alone records v2", []string{v2}, 1, v2},
		{"v1 only offered records v1", []string{protocol.DaemonCapabilityTaskSupplementV1, v1}, 1, v1},
		{"no PR event offer records no row", []string{protocol.DaemonCapabilityTaskSupplementV1}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID, started := startOffering(t, tc.offered)
			rows, stored := storedPREventCapability(t, taskID)
			if rows != tc.wantRows || stored != tc.want {
				t.Fatalf("stored PR event capability = %d rows %q, want %d rows %q", rows, stored, tc.wantRows, tc.want)
			}
			if started.PREventCapability != tc.want {
				t.Fatalf("echoed pr_event_capability = %q, want the stored %q", started.PREventCapability, tc.want)
			}
		})
	}
}

// A v2 task takes the same lifecycle notices as a v1 task: ListRunningPREventTaskIDs
// and EnqueueTaskPREventDelivery accept either capability.
func TestPRLifecycleNoticeReachesTaskWithEitherCapability(t *testing.T) {
	for _, capability := range []string{
		protocol.DaemonCapabilityTaskSupplementPREventV1,
		protocol.DaemonCapabilityTaskSupplementPREventV2,
	} {
		t.Run(capability, func(t *testing.T) {
			w := newC1World(t)
			taskID := dbfx.Task(t, w.agentID, testutil.Cols{
				"issue_id": w.issueID, "runtime_id": w.runtimeID, "status": "running",
				"dispatched_at": testutil.Raw("now()"), "started_at": testutil.Raw("now()"),
			})
			dbfx.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
				taskID, testWorkspaceID, w.issueID, capability)
			dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, taskID)

			w.fire(t, "converted_to_draft", "open", false, true)

			if n := dbfx.Count(t, `SELECT count(*) FROM task_pr_event_delivery WHERE task_id = $1 AND item_kind = 'pr_event' AND status = 'pending'`, taskID); n != 1 {
				t.Fatalf("pending PR items for a %s task = %d, want 1", capability, n)
			}
		})
	}
}

// Golden: a v2 task claims and acks a PR item with the same body as a v1 task.
func TestTaskPREventV2ClaimAndAckMatchV1(t *testing.T) {
	bodies := map[string]map[string]any{}
	for _, capability := range []string{
		protocol.DaemonCapabilityTaskSupplementPREventV1,
		protocol.DaemonCapabilityTaskSupplementPREventV2,
	} {
		t.Run(capability, func(t *testing.T) {
			f := newPREventFixture(t)
			dbfx.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
				f.taskID, testWorkspaceID, f.issueID, capability)
			dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, f.taskID)
			if err := f.enqueue(t); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			body := f.claim(t)
			if len(body) == 0 {
				t.Fatal("claimed nothing")
			}
			f.ack(t, true, http.StatusOK)
			if s := f.eventStatus(t); s != "delivered" {
				t.Fatalf("event status after ack = %q, want delivered", s)
			}
			bodies[capability] = body
		})
	}
	v1Body, v2Body := bodies[protocol.DaemonCapabilityTaskSupplementPREventV1], bodies[protocol.DaemonCapabilityTaskSupplementPREventV2]
	if v1Body == nil || v2Body == nil {
		t.Fatal("a capability produced no claim body")
	}
	if len(v1Body) != len(v2Body) {
		t.Fatalf("claim bodies differ in shape: v1=%v v2=%v", v1Body, v2Body)
	}
	for key, want := range v1Body {
		got, ok := v2Body[key]
		if !ok {
			t.Fatalf("v2 claim lacks %q: v1=%v v2=%v", key, v1Body, v2Body)
		}
		switch key {
		case "event_id", "pr_number": // per-fixture identifiers
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("claim field %q: v1=%v v2=%v", key, want, got)
		}
	}
}
