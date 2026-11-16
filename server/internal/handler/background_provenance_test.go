package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DM-task provenance (CHE-4013 step 9, CHE-4104). A background task has no
// chat_session_id; source_chat_session_id names the DM it came from, and that
// DM's creator is the only member who may read the task's result and error.

func TestBackgroundEnqueueRejectsDMTaskWithoutSourceSession(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bgp-guard")
	enqueue := func(dm string) (db.AgentTaskQueue, error) {
		in := service.BackgroundTaskInput{
			Job: bgagents.DMTitle, WorkspaceID: util.MustParseUUID(wsID), AgentID: util.MustParseUUID(a),
			MemberID: util.MustParseUUID(testUserID), Source: bgagents.UserSource, Slot: bgagents.Primary, Input: "hi",
		}
		if dm != "" {
			in.SourceChatSessionID = util.MustParseUUID(dm)
		}
		task, err := testHandler.TaskService.EnqueueBackgroundTask(context.Background(), in)
		if err == nil {
			dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", util.UUIDToString(task.ID))
		}
		return task, err
	}
	if _, err := enqueue(""); !errors.Is(err, service.ErrBackgroundMissingSourceSession) {
		t.Fatalf("enqueue without a source session = %v, want ErrBackgroundMissingSourceSession", err)
	}
	dm := dbfx.ChatSession(t, a, testutil.Cols{"workspace_id": wsID})
	task, err := enqueue(dm)
	if err != nil {
		t.Fatalf("enqueue with a source session: %v", err)
	}
	var got string
	dbfx.QueryRow(t, `SELECT source_chat_session_id::text FROM agent_task_queue WHERE id=$1`, util.UUIDToString(task.ID)).Scan(&got)
	if got != dm {
		t.Fatalf("source_chat_session_id = %q, want %q", got, dm)
	}
	var chat *string
	dbfx.QueryRow(t, `SELECT chat_session_id::text FROM agent_task_queue WHERE id=$1`, util.UUIDToString(task.ID)).Scan(&chat)
	if chat != nil {
		t.Fatalf("chat_session_id = %q, want NULL: the DM must not serialize or claim as a chat task", *chat)
	}
}

func TestBackgroundRetryCloneKeepsSourceSession(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bgp-retry")
	parent, err := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.UserSource)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	child, err := testHandler.Queries.CreateRetryTask(context.Background(), db.CreateRetryTaskParams{ID: parent.ID})
	if err != nil {
		t.Fatalf("retry clone: %v", err)
	}
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", util.UUIDToString(child.ID))
	var want, got *string
	dbfx.QueryRow(t, `SELECT source_chat_session_id::text FROM agent_task_queue WHERE id=$1`, util.UUIDToString(parent.ID)).Scan(&want)
	dbfx.QueryRow(t, `SELECT source_chat_session_id::text FROM agent_task_queue WHERE id=$1`, util.UUIDToString(child.ID)).Scan(&got)
	if want == nil || got == nil || *got != *want {
		t.Fatalf("retry clone source_chat_session_id = %v, want %v", got, want)
	}
}

func TestAgentTaskListsRedactForeignBackgroundTaskContent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ownerID := dbfx.User(t, "Background owner", "bg-task-privacy-owner@example.test")
	dbfx.Member(t, testWorkspaceID, ownerID, "member")
	runtimeID := handlerTestRuntimeID(t)
	agentID := dbfx.Agent(t, "bg task privacy agent", runtimeID, testutil.Cols{"owner_id": ownerID})
	dm := dbfx.ChatSession(t, agentID, testutil.Cols{"creator_id": ownerID})
	bg := func(cols testutil.Cols) string {
		base := testutil.Cols{
			"runtime_id": runtimeID, "status": "completed", "completed_at": testutil.Raw("now()"),
			"purpose": "bg:dm_title",
			"result":  testutil.Raw(`'{"output":"private title"}'::jsonb`), "error": "private bg error",
		}
		for k, v := range cols {
			base[k] = v
		}
		return dbfx.Task(t, agentID, base)
	}
	derived := bg(testutil.Cols{"source_chat_session_id": dm})
	legacy := bg(testutil.Cols{"status": "running", "started_at": testutil.Raw("now()")}) // a bg:* row with no recorded DM cannot be resolved: foreign

	lists := map[string]func(t *testing.T, userID string) []AgentTaskResponse{
		"agent tasks": func(t *testing.T, userID string) []AgentTaskResponse {
			req := withURLParam(newRequestAs(userID, http.MethodGet, "/api/agents/"+agentID+"/tasks", nil), "id", agentID)
			var tasks []AgentTaskResponse
			testutil.Call(t, testHandler.ListAgentTasks, req).Want(http.StatusOK).JSON(&tasks)
			return tasks
		},
		"workspace snapshot": func(t *testing.T, userID string) []AgentTaskResponse {
			req := newRequestAs(userID, http.MethodGet, "/api/agent-task-snapshot", nil)
			var tasks []AgentTaskResponse
			testutil.Call(t, testHandler.ListWorkspaceAgentTaskSnapshot, req).Want(http.StatusOK).JSON(&tasks)
			return tasks
		},
	}
	for name, list := range lists {
		t.Run(name, func(t *testing.T) {
			find := func(viewer, id string) AgentTaskResponse {
				for _, task := range list(t, viewer) {
					if task.ID == id {
						return task
					}
				}
				t.Fatalf("viewer %s: task %s missing; rows must stay listed for presence", viewer, id)
				return AgentTaskResponse{}
			}
			own := find(ownerID, derived)
			if own.Result == nil || own.Error == nil || *own.Error != "private bg error" {
				t.Fatalf("DM owner sees result=%v error=%v, want full content", own.Result, own.Error)
			}
			for _, id := range []string{derived, legacy} {
				foreign := find(testUserID, id)
				if foreign.Result != nil || foreign.Error != nil {
					t.Fatalf("foreign viewer sees bg task %s result=%v error=%v, want both redacted", id, foreign.Result, foreign.Error)
				}
				if foreign.Status == "" || foreign.AgentID != agentID {
					t.Fatalf("foreign viewer row = %+v, want the row metadata kept", foreign)
				}
			}
		})
	}
}
