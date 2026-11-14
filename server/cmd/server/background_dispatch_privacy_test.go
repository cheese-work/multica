package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// task:dispatch fans out to the whole workspace and carries the task context.
// For a DM-derived background task that context holds the DM's text, so member
// B must get the dispatch (workload metadata) without the input (CHE-4105,
// spec §Privacy of DM-derived tasks).
func TestBackgroundTaskDispatchHidesDMInputFromBystander(t *testing.T) {
	ctx := context.Background()
	fx := workspaceFixture(t)
	bystanderID := fx.User(t, "Dispatch bystander", "bg-dispatch-bystander@example.test")
	fx.Member(t, testWorkspaceID, bystanderID, "member")
	// Only a daemon advertising the runner is handed a background task.
	runtimeID := fx.Runtime(t, "bg dispatch privacy runtime", testutil.Cols{
		"metadata": testutil.Raw(`'{"capabilities":["` + protocol.DaemonCapabilityBackgroundOutputOnlyV1 + `"]}'::jsonb`),
	})
	agentID := fx.Agent(t, "bg dispatch privacy agent", runtimeID, testutil.Cols{"owner_id": testUserID})
	dm := fx.ChatSession(t, agentID, testutil.Cols{"creator_id": testUserID})

	hub := realtime.NewHub()
	go hub.Run()
	bus := events.New()
	registerListeners(bus, hub)
	router, h := NewRouterWithOptions(testPool, hub, bus, analytics.NoopClient{}, nil, RouterOptions{})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	owner, _ := realtimeMembershipConn(t, server, testUserID, testWorkspaceID)
	bystander, _ := realtimeMembershipConn(t, server, bystanderID, testWorkspaceID)

	const secret = "bgdispatchsecret"
	var labels []string
	for _, job := range []bgagents.Job{bgagents.DMTitle, bgagents.QuickActions, bgagents.Compact} {
		for _, slot := range []bgagents.SlotName{bgagents.Primary, bgagents.Fallback} {
			label := string(job) + "/" + string(slot)
			labels = append(labels, label)
			ctxJSON, _ := json.Marshal(service.BackgroundTaskContext{
				Type: service.BackgroundTaskContextType, Job: string(job), WorkspaceID: testWorkspaceID,
				Slot: string(slot), Input: secret + " " + label, Guard: secret + " guard " + label,
			})
			cols := testutil.Cols{
				"runtime_id": runtimeID, "purpose": bgagents.Purpose(job), "source_chat_session_id": dm,
				"context": testutil.Raw("'" + string(ctxJSON) + "'::jsonb"),
			}
			if job == bgagents.Compact {
				cols["chat_session_id"] = dm
			}
			taskID := fx.Task(t, agentID, cols)
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id=$1`, taskID)
			})
			claimed, err := h.TaskService.ClaimTask(ctx, util.MustParseUUID(agentID))
			if err != nil || claimed == nil || util.UUIDToString(claimed.ID) != taskID {
				t.Fatalf("%s: claim = %v, %v; want task %s", label, claimed, err, taskID)
			}
			// Free the agent's slot so the next attempt can be claimed.
			testPool.Exec(ctx, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, taskID)
		}
	}

	bus.Publish(events.Event{Type: protocol.EventIssueUpdated, WorkspaceID: testWorkspaceID, Payload: map[string]string{}})
	for name, conn := range map[string]*struct{ frames []chatPrivacyFrame }{
		"owner":     {readChatPrivacyFrames(t, owner, protocol.EventIssueUpdated)},
		"bystander": {readChatPrivacyFrames(t, bystander, protocol.EventIssueUpdated)},
	} {
		dispatches := 0
		for _, f := range conn.frames {
			if f.Type != protocol.EventTaskDispatch {
				continue
			}
			dispatches++
			if name == "bystander" && strings.Contains(f.raw, secret) {
				t.Errorf("bystander task:dispatch leaked the DM input or guard: %s", f.raw)
			}
		}
		if dispatches != len(labels) {
			t.Errorf("%s got %d task:dispatch frames, want %d (metadata must still reach presence views)", name, dispatches, len(labels))
		}
	}
}
