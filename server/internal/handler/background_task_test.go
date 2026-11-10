package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func enqueueBG(t *testing.T, wsID, agentID string, job bgagents.Job, src bgagents.Source) (db.AgentTaskQueue, error) {
	t.Helper()
	task, err := testHandler.TaskService.EnqueueBackgroundTask(context.Background(), service.BackgroundTaskInput{
		Job: job, WorkspaceID: util.MustParseUUID(wsID), AgentID: util.MustParseUUID(agentID),
		MemberID: util.MustParseUUID(testUserID), Source: src, Slot: bgagents.Primary, Input: "hello",
	})
	if err == nil {
		dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", util.UUIDToString(task.ID))
	}
	return task, err
}

func bgTaskStatus(t *testing.T, id db.AgentTaskQueue) string {
	t.Helper()
	var s string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id=$1`, util.UUIDToString(id.ID)).Scan(&s)
	return s
}

func TestBackgroundTaskClaimRefusedForForeignRuntime(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bgt-claim")
	task, err := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.UserSource)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	foreign := dbfx.Runtime(t, "bgt-claim-foreign", testutil.Cols{"workspace_id": wsID})
	_, err = testHandler.Queries.ClaimAgentTask(context.Background(), db.ClaimAgentTaskParams{
		AgentID: task.AgentID, RuntimeID: util.MustParseUUID(foreign), PrepareLeaseSecs: 60, RuntimeStaleSecs: 600,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim by a foreign runtime = %v, want no rows", err)
	}
	if got, err := testHandler.TaskService.ClaimTaskForRuntime(context.Background(), util.MustParseUUID(foreign)); err != nil || got != nil {
		t.Fatalf("foreign runtime claimed %v (err %v)", got, err)
	}
	if bgTaskStatus(t, task) != "queued" {
		t.Fatal("the task left queued")
	}
}

func TestBackgroundTaskRecordsMemberAndChainSource(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bgt-ctx")
	task, err := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.WorkspaceSource)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var ctxJSON map[string]any
	var purpose string
	var issue, chat any
	dbfx.QueryRow(t, `SELECT context, purpose, issue_id, chat_session_id FROM agent_task_queue WHERE id=$1`, util.UUIDToString(task.ID)).Scan(&ctxJSON, &purpose, &issue, &chat)
	want := map[string]any{
		"chain_source": "workspace", "member_id": testUserID, "slot": "primary", "job": "dm_title",
		"claim_deadline_seconds": float64(10), "run_deadline_seconds": float64(60), "input": "hello",
	}
	for k, v := range want {
		if ctxJSON[k] != v {
			t.Errorf("context[%s] = %v, want %v", k, ctxJSON[k], v)
		}
	}
	if purpose != "bg:dm_title" || issue != nil || chat != nil {
		t.Errorf("purpose=%q issue=%v chat=%v, want bg:dm_title with no links", purpose, issue, chat)
	}
}

func TestBackgroundTaskRefusesChatCompaction(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bgt-compact")
	if task, err := enqueueBG(t, wsID, a, bgagents.Compact, bgagents.UserSource); err == nil {
		t.Fatalf("enqueued a chat_compaction job: %v", task.ID)
	}
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1`, a).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows written for a refused job", n)
	}
}

func TestWorkspaceBackgroundAgentChangeCancelsWorkspaceSourcedTasks(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bgt-cancel")
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": a}, "quick_actions": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	wsTitle, _ := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.WorkspaceSource)
	userTitle, _ := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.UserSource)
	wsQuick, _ := enqueueBG(t, wsID, a, bgagents.QuickActions, bgagents.WorkspaceSource)

	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": b}, "quick_actions": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)

	for _, c := range []struct {
		name string
		task db.AgentTaskQueue
		want string
	}{
		{"workspace-sourced dm_title", wsTitle, "cancelled"},
		{"user-sourced dm_title", userTitle, "queued"},
		{"workspace-sourced quick_actions (job unchanged)", wsQuick, "queued"},
	} {
		if got := bgTaskStatus(t, c.task); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestBackgroundTaskCompletionDispatchesByPurpose(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bgt-done")
	var got []string
	svc := testHandler.TaskService
	prev := svc.BackgroundWriters
	svc.BackgroundWriters = map[string]service.BackgroundWriter{
		bgagents.Purpose(bgagents.DMTitle): func(_ context.Context, _ *db.Queries, task db.AgentTaskQueue, result []byte) error {
			got = append(got, string(result))
			if string(result) == `"bad"` {
				return service.ErrInvalidBackgroundOutput
			}
			return nil
		},
	}
	t.Cleanup(func() { svc.BackgroundWriters = prev })

	for _, out := range []string{`"good"`, `"bad"`} {
		task, err := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.UserSource)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, util.UUIDToString(task.ID))
		if _, _, err := svc.CompleteTaskWithRunContext(context.Background(), task.ID, []byte(out), "", "", "", false, "", "", nil); err != nil {
			t.Fatalf("complete %s: %v", out, err)
		}
		if s := bgTaskStatus(t, task); s != "completed" {
			t.Fatalf("status after %s = %s, want completed (an invalid output still completes the task)", out, s)
		}
	}
	if len(got) != 2 || got[0] != `"good"` || got[1] != `"bad"` {
		t.Fatalf("writer saw %v, want both outputs in order", got)
	}
}
