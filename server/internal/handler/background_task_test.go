package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
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

// CHE-4100: the server hands an output_only task only to a runtime whose daemon
// advertises the capability; a chat_compaction-shaped task keeps today's claim.
func TestOutputOnlyTaskClaimNeedsDaemonCapability(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bgt-cap")
	var rt string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id=$1`, a).Scan(&rt)
	rtID := util.MustParseUUID(rt)

	bgTask, err := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.UserSource)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var compactID string
	dbfx.QueryRow(t, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, context, purpose)
		VALUES ($1, $2, 'queued', '{"type":"compact"}'::jsonb, 'bg:compact') RETURNING id`, b, rt).Scan(&compactID)
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", compactID)

	// A daemon without the capability: the compaction task is still claimed, the output_only task never.
	got, err := testHandler.TaskService.ClaimTaskForRuntime(context.Background(), rtID)
	if err != nil || got == nil || util.UUIDToString(got.ID) != compactID {
		t.Fatalf("capability-less claim = %v (err %v), want the compaction task %s", got, err, compactID)
	}
	if again, err := testHandler.TaskService.ClaimTaskForRuntime(context.Background(), rtID); err != nil || again != nil {
		t.Fatalf("capability-less daemon claimed %v (err %v), want nothing", again, err)
	}
	if bgTaskStatus(t, bgTask) != "queued" {
		t.Fatal("the output_only task left queued without the capability")
	}

	// The same runtime once its daemon advertises the capability.
	dbfx.Exec(t, `UPDATE agent_runtime SET metadata=jsonb_build_object('capabilities', jsonb_build_array($2::text)) WHERE id=$1`, rt, protocol.DaemonCapabilityBackgroundOutputOnlyV1)
	got, err = testHandler.TaskService.ClaimTaskForRuntime(context.Background(), rtID)
	if err != nil || got == nil || got.ID != bgTask.ID {
		t.Fatalf("capable claim = %v (err %v), want the output_only task", got, err)
	}
}

// CHE-4100: the claim response carries the job, the bounded input and the run
// deadline, and the workspace check passes without an issue, chat or autopilot.
func TestOutputOnlyTaskClaimResponseCarriesInput(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bgt-resp")
	var rt string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id=$1`, a).Scan(&rt)
	dbfx.Exec(t, `UPDATE agent_runtime SET metadata=jsonb_build_object('capabilities', jsonb_build_array($2::text)) WHERE id=$1`, rt, protocol.DaemonCapabilityBackgroundOutputOnlyV1)
	bgTask, err := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.UserSource)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+rt+"/tasks/claim", nil), "runtimeId", rt)
	var resp struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.ID != util.UUIDToString(bgTask.ID) {
		t.Fatalf("claim = %+v, want the output_only task", resp.Task)
	}
	if resp.Task.BackgroundJob != "dm_title" || resp.Task.BackgroundInput != "hello" || resp.Task.BackgroundRunSeconds != 60 || resp.Task.WorkspaceID != wsID {
		t.Fatalf("claim response = job %q input %q run %d ws %q", resp.Task.BackgroundJob, resp.Task.BackgroundInput, resp.Task.BackgroundRunSeconds, resp.Task.WorkspaceID)
	}
}

// CHE-4100: stale-dispatch recovery must apply the same capability gate as the
// queued claim. A background task dispatched to a runtime that has since lost
// the capability is never re-delivered; a compaction task still is.
func TestStaleReclaimOfBackgroundTaskNeedsDaemonCapability(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bgt-reclaim")
	var rt string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id=$1`, a).Scan(&rt)
	rtID := util.MustParseUUID(rt)

	bgTask, err := enqueueBG(t, wsID, a, bgagents.DMTitle, bgagents.UserSource)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var compactID string
	dbfx.QueryRow(t, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, context, purpose)
		VALUES ($1, $2, 'queued', '{"type":"compact"}'::jsonb, 'bg:compact') RETURNING id`, b, rt).Scan(&compactID)
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", compactID)
	// Both tasks were dispatched long ago and never started; the runtime has no capability now.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='dispatched', dispatched_at=now()-interval '1 hour' WHERE id = ANY($1::uuid[])`,
		[]string{util.UUIDToString(bgTask.ID), compactID})

	q := testHandler.Queries
	one, err := q.ReclaimStaleDispatchedTaskForRuntime(context.Background(), db.ReclaimStaleDispatchedTaskForRuntimeParams{
		RuntimeID: rtID, PrepareLeaseSecs: 60, ClaimRecoverySecs: 30, RuntimeStaleSecs: 600,
	})
	if err != nil || util.UUIDToString(one.ID) != compactID {
		t.Fatalf("singular reclaim = %v (err %v), want only the compaction task %s", one.ID, err, compactID)
	}
	if _, err := q.ReclaimStaleDispatchedTaskForRuntime(context.Background(), db.ReclaimStaleDispatchedTaskForRuntimeParams{
		RuntimeID: rtID, PrepareLeaseSecs: 60, ClaimRecoverySecs: 0, RuntimeStaleSecs: 600,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("singular reclaim returned the output_only task without the capability (err %v)", err)
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET dispatched_at=now()-interval '1 hour', prepare_lease_expires_at=NULL WHERE id = ANY($1::uuid[])`,
		[]string{util.UUIDToString(bgTask.ID), compactID})
	batch, err := q.ReclaimStaleDispatchedTasksForRuntimes(context.Background(), db.ReclaimStaleDispatchedTasksForRuntimesParams{
		RuntimeIds: []pgtype.UUID{rtID}, ClaimRecoverySecs: 30, RuntimeStaleSecs: 600, MaxTasks: 10, PrepareLeaseSecs: 60,
	})
	if err != nil || len(batch) != 1 || util.UUIDToString(batch[0].ID) != compactID {
		t.Fatalf("batch reclaim = %d rows (err %v), want only the compaction task %s", len(batch), err, compactID)
	}

	// Once the daemon advertises the capability, both recovery paths deliver the task again.
	dbfx.Exec(t, `UPDATE agent_runtime SET metadata=jsonb_build_object('capabilities', jsonb_build_array($2::text)) WHERE id=$1`, rt, protocol.DaemonCapabilityBackgroundOutputOnlyV1)
	dbfx.Exec(t, `UPDATE agent_task_queue SET dispatched_at=now()-interval '1 hour', prepare_lease_expires_at=NULL WHERE id=$1`, util.UUIDToString(bgTask.ID))
	got, err := q.ReclaimStaleDispatchedTaskForRuntime(context.Background(), db.ReclaimStaleDispatchedTaskForRuntimeParams{
		RuntimeID: rtID, PrepareLeaseSecs: 60, ClaimRecoverySecs: 30, RuntimeStaleSecs: 600,
	})
	if err != nil || got.ID != bgTask.ID {
		t.Fatalf("capable singular reclaim = %v (err %v), want the output_only task", got.ID, err)
	}
}
