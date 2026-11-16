package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Two-member regression for the task-content routes (CHE-4013 step 10,
// CHE-4105, spec §Privacy of DM-derived tasks). Member A owns the DM; member B
// shares the workspace. B gets row metadata only, on every route that can carry
// a DM-derived task's content: A gets everything.

const bgSecret = "bgsecret"

type bgPrivacyTask struct{ id, label string }

type bgPrivacyFixture struct {
	ownerID, agentID, dm, runtimeID string
	tasks                           []bgPrivacyTask
}

// seedBackgroundPrivacy writes a failed primary and a fallback attempt for each
// DM-derived job. Compaction rows carry chat_session_id and
// source_chat_session_id: the DM itself.
func seedBackgroundPrivacy(t *testing.T) bgPrivacyFixture {
	t.Helper()
	ownerID := dbfx.User(t, "Background route owner", "bg-route-privacy-owner@example.test")
	dbfx.Member(t, testWorkspaceID, ownerID, "member")
	runtimeID := handlerTestRuntimeID(t)
	agentID := dbfx.Agent(t, "bg route privacy agent", runtimeID, testutil.Cols{"owner_id": ownerID})
	dm := dbfx.ChatSession(t, agentID, testutil.Cols{"creator_id": ownerID})
	f := bgPrivacyFixture{ownerID: ownerID, agentID: agentID, dm: dm, runtimeID: runtimeID}
	for _, job := range []bgagents.Job{bgagents.DMTitle, bgagents.QuickActions, bgagents.Compact} {
		for _, slot := range []bgagents.SlotName{bgagents.Primary, bgagents.Fallback} {
			label := string(job) + "/" + string(slot)
			ctxJSON, _ := json.Marshal(service.BackgroundTaskContext{
				Type: service.BackgroundTaskContextType, Job: string(job), WorkspaceID: testWorkspaceID,
				Slot: string(slot), Input: bgSecret + " input " + label,
			})
			cols := testutil.Cols{
				"runtime_id": runtimeID, "status": "failed", "completed_at": testutil.Raw("now()"),
				"purpose": bgagents.Purpose(job), "source_chat_session_id": dm,
				"context": testutil.Raw("'" + string(ctxJSON) + "'::jsonb"),
				"result":  testutil.Raw(`'{"output":"` + bgSecret + ` result"}'::jsonb`),
				"error":   bgSecret + " error " + label,
			}
			if job == bgagents.Compact {
				cols["chat_session_id"] = dm
			}
			id := dbfx.Task(t, agentID, cols)
			dbfx.Insert(t, "task_message", testutil.Cols{"task_id": id, "seq": 1, "type": "text", "content": bgSecret + " transcript " + label})
			f.tasks = append(f.tasks, bgPrivacyTask{id: id, label: label})
		}
	}
	return f
}

func TestBackgroundTaskContentRoutesAreOwnerOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := seedBackgroundPrivacy(t)

	t.Run("messages", func(t *testing.T) {
		for _, task := range f.tasks {
			read := func(userID string) *testutil.Response {
				req := withURLParam(newRequestAs(userID, http.MethodGet, "/api/tasks/"+task.id+"/messages", nil), "taskId", task.id)
				req = req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, db.Member{}))
				return testutil.Call(t, testHandler.ListTaskMessagesByUser, req)
			}
			var history []protocol.TaskMessagePayload
			read(f.ownerID).Want(http.StatusOK).JSON(&history)
			if len(history) != 1 || !strings.Contains(history[0].Content, bgSecret) {
				t.Errorf("%s: owner transcript = %+v, want the one private message", task.label, history)
			}
			if resp := read(testUserID); resp.Code != http.StatusNotFound || strings.Contains(resp.Text(), bgSecret) {
				t.Errorf("%s: foreign transcript = %d %s, want 404 with no content", task.label, resp.Code, resp.Text())
			}
		}
	})

	// The snapshot lists one latest outcome per agent, so it cannot show every
	// row: it only has to show the rows it does return without content.
	lists := map[string]struct {
		all  bool
		call func(t *testing.T, userID string) *testutil.Response
	}{
		"agent tasks": {true, func(t *testing.T, userID string) *testutil.Response {
			req := withURLParam(newRequestAs(userID, http.MethodGet, "/api/agents/"+f.agentID+"/tasks", nil), "id", f.agentID)
			return testutil.Call(t, testHandler.ListAgentTasks, req).Want(http.StatusOK)
		}},
		"workspace snapshot": {false, func(t *testing.T, userID string) *testutil.Response {
			req := newRequestAs(userID, http.MethodGet, "/api/agent-task-snapshot", nil)
			return testutil.Call(t, testHandler.ListWorkspaceAgentTaskSnapshot, req).Want(http.StatusOK)
		}},
	}
	for name, list := range lists {
		t.Run(name, func(t *testing.T) {
			var own, foreign []AgentTaskResponse
			ownResp, foreignResp := list.call(t, f.ownerID), list.call(t, testUserID)
			ownResp.JSON(&own)
			foreignResp.JSON(&foreign)
			if !strings.Contains(ownResp.Text(), bgSecret) {
				t.Fatalf("owner response has no private content; the fixture is wrong")
			}
			if strings.Contains(foreignResp.Text(), bgSecret) {
				t.Errorf("foreign response carries DM-derived content: %s", foreignResp.Text())
			}
			seen := map[string]bool{}
			for _, task := range foreign {
				seen[task.ID] = true
			}
			for _, task := range f.tasks {
				if list.all && !seen[task.id] {
					t.Errorf("%s: row missing for the foreign viewer; metadata must stay listed", task.label)
				}
			}
		})
	}

	t.Run("cancel", func(t *testing.T) {
		task := dbfx.Task(t, f.agentID, testutil.Cols{
			"runtime_id": f.runtimeID, "status": "queued", "purpose": bgagents.Purpose(bgagents.DMTitle),
			"source_chat_session_id": f.dm,
			"result":                 testutil.Raw(`'{"output":"` + bgSecret + ` result"}'::jsonb`), "error": bgSecret + " error",
		})
		req := withURLParam(newRequestAs(testUserID, http.MethodPost, "/api/tasks/"+task+"/cancel", nil), "taskId", task)
		resp := testutil.Call(t, testHandler.CancelTaskByUser, withChatTestWorkspaceCtx(t, req))
		if resp.Code < 400 || strings.Contains(resp.Text(), bgSecret) {
			t.Errorf("foreign cancel = %d %s, want a refusal with no content", resp.Code, resp.Text())
		}
		if got := taskStatus(t, task); got != "queued" {
			t.Errorf("foreign cancel changed the task to %q", got)
		}
	})
}

func TestBackgroundTaskMessageEventsRouteToTheDMOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := seedBackgroundPrivacy(t)
	legacy := dbfx.Task(t, f.agentID, testutil.Cols{
		"runtime_id": f.runtimeID, "status": "running", "started_at": testutil.Raw("now()"),
		"purpose": bgagents.Purpose(bgagents.DMTitle),
		"context": testutil.Raw(`'{"type":"background","job":"dm_title","workspace_id":"` + testWorkspaceID + `"}'::jsonb`),
	})

	sharedBus := testHandler.Bus
	testHandler.Bus = events.New()
	t.Cleanup(func() { testHandler.Bus = sharedBus })
	var mu sync.Mutex
	published := map[string][]events.Event{}
	testHandler.Bus.Subscribe(protocol.EventTaskMessage, func(e events.Event) {
		mu.Lock()
		published[e.TaskID] = append(published[e.TaskID], e)
		mu.Unlock()
	})

	for _, task := range append(f.tasks, bgPrivacyTask{id: legacy, label: "legacy"}) {
		testutil.Call(t, testHandler.ReportTaskMessages, batchMessagesRequest(t, task.id, []any{
			map[string]any{"seq": 2, "type": "text", "content": bgSecret + " live"},
		})).Want(http.StatusOK)
		mu.Lock()
		got := published[task.id]
		mu.Unlock()
		if task.label == "legacy" {
			// No resolvable owner: nothing may reach the workspace room.
			for _, e := range got {
				if e.ChatSessionID == "" {
					t.Errorf("legacy: event published without a private scope: %+v", e)
				}
			}
			continue
		}
		if len(got) != 1 || got[0].ChatSessionID != f.dm || got[0].ChatSessionOwnerID != f.ownerID {
			t.Errorf("%s: events = %+v, want one scoped to DM %s and owner %s", task.label, got, f.dm, f.ownerID)
		}
	}
}
