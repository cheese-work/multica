package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Personal writes cancel the member's tasks (CHE-4114).

func (f memberBG) enqueueFor(t *testing.T, member, agent string, job bgagents.Job, src bgagents.Source) db.AgentTaskQueue {
	t.Helper()
	task, err := testHandler.TaskService.EnqueueBackgroundTask(context.Background(), service.BackgroundTaskInput{
		Job: job, WorkspaceID: util.MustParseUUID(f.ws), AgentID: util.MustParseUUID(agent),
		MemberID: util.MustParseUUID(member), Source: src, Slot: bgagents.Primary, Input: "hello",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", util.UUIDToString(task.ID))
	return task
}

func (f memberBG) wantStatus(t *testing.T, name string, task db.AgentTaskQueue, want string) {
	t.Helper()
	if got := bgTaskStatus(t, task); got != want {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}

func title(agent string) map[string]any {
	return map[string]any{"dm_title": map[string]any{"agent_id": agent}}
}

func TestPersonalWriteCancelsOnlyTheMembersTasksOfThatJob(t *testing.T) {
	f := memberBGFixture(t, "pcx-scope")
	f.setWorkspace(t, map[string]any{"dm_title": map[string]any{"agent_id": f.a1}, "quick_actions": map[string]any{"agent_id": f.a1}})

	// Workspace X -> personal Y before claim: A's queued X task is cancelled,
	// B's workspace-sourced task, A's other job and A's other job's task stay.
	aX := f.enqueueFor(t, testUserID, f.a1, bgagents.DMTitle, bgagents.WorkspaceSource)
	bX := f.enqueueFor(t, f.memberB, f.a1, bgagents.DMTitle, bgagents.WorkspaceSource)
	aQuick := f.enqueueFor(t, testUserID, f.a1, bgagents.QuickActions, bgagents.WorkspaceSource)
	f.put(t, testUserID, title(f.a2), nil).Want(http.StatusOK)
	f.wantStatus(t, "A's workspace-sourced task after X -> Y", aX, "cancelled")
	f.wantStatus(t, "B's workspace-sourced task", bX, "queued")
	f.wantStatus(t, "A's other job", aQuick, "queued")

	// Clear back to the workspace default: A's queued Y task is cancelled.
	aY := f.enqueueFor(t, testUserID, f.a2, bgagents.DMTitle, bgagents.UserSource)
	f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": nil}}, nil).Want(http.StatusOK)
	f.wantStatus(t, "A's user-sourced task after clear", aY, "cancelled")
	f.wantStatus(t, "B's task after A's clear", bX, "queued")

	// Reset drops the job: same cancel.
	f.put(t, testUserID, title(f.a2), nil).Want(http.StatusOK)
	aY2 := f.enqueueFor(t, testUserID, f.a2, bgagents.DMTitle, bgagents.UserSource)
	f.reset(t, testUserID, "dm_title", nil).Want(http.StatusOK)
	f.wantStatus(t, "A's user-sourced task after reset", aY2, "cancelled")
	f.wantStatus(t, "B's task after A's reset", bX, "queued")

	// An unchanged write is not effective and cancels nothing.
	aQuick2 := f.enqueueFor(t, testUserID, f.a1, bgagents.QuickActions, bgagents.WorkspaceSource)
	f.put(t, testUserID, map[string]any{}, nil).Want(http.StatusOK)
	f.wantStatus(t, "task after unchanged write", aQuick2, "queued")
}

func TestPersonalWriteFallbackSlotChangeCancels(t *testing.T) {
	f := memberBGFixture(t, "pcx-slot")
	f.put(t, testUserID, title(f.a1), nil).Want(http.StatusOK)
	task := f.enqueueFor(t, testUserID, f.a1, bgagents.DMTitle, bgagents.UserSource)
	// Only the fallback slot changes; either slot counts.
	f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"fallback_agent_id": f.a2}}, nil).Want(http.StatusOK)
	f.wantStatus(t, "task after a fallback-only change", task, "cancelled")
}

func TestPersonalWriteCancelIsPermanentAcrossAgentRoundTrip(t *testing.T) {
	f := memberBGFixture(t, "pcx-roundtrip")
	var wrote []string
	svc := testHandler.TaskService
	prev := svc.BackgroundWriters
	svc.BackgroundWriters = map[string]service.BackgroundWriter{
		bgagents.Purpose(bgagents.DMTitle): func(_ context.Context, _ *db.Queries, _ db.AgentTaskQueue, result []byte) error {
			wrote = append(wrote, string(result))
			return nil
		},
	}
	t.Cleanup(func() { svc.BackgroundWriters = prev })

	f.put(t, testUserID, title(f.a1), nil).Want(http.StatusOK)
	first := f.enqueueFor(t, testUserID, f.a1, bgagents.DMTitle, bgagents.UserSource)
	f.put(t, testUserID, title(f.a2), nil).Want(http.StatusOK)
	f.put(t, testUserID, title(f.a1), nil).Want(http.StatusOK) // A -> B -> A
	f.wantStatus(t, "task queued under the first A", first, "cancelled")

	dbfx.Exec(t, `UPDATE agent_task_queue SET started_at=now() WHERE id=$1`, util.UUIDToString(first.ID))
	_, _, _ = svc.CompleteTaskWithRunContext(context.Background(), first.ID, []byte(`"late"`), "", "", "", false, "", "", nil)
	f.wantStatus(t, "task after a late completion", first, "cancelled")
	if len(wrote) != 0 {
		t.Fatalf("a cancelled attempt's completion wrote %v", wrote)
	}
}

func TestPersonalWriteRollsBackWhenCancelFails(t *testing.T) {
	f := memberBGFixture(t, "pcx-rollback")
	f.put(t, testUserID, title(f.a1), nil).Want(http.StatusOK)
	task := f.enqueueFor(t, testUserID, f.a1, bgagents.DMTitle, bgagents.UserSource)
	acts := len(f.activity(t))
	dbfx.Exec(t, `CREATE FUNCTION pcx_fail_cancel() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected cancel failure'; END $$`)
	dbfx.Exec(t, `CREATE TRIGGER pcx_fail_cancel BEFORE UPDATE ON agent_task_queue FOR EACH ROW WHEN (NEW.status = 'cancelled' AND OLD.status <> 'cancelled') EXECUTE FUNCTION pcx_fail_cancel()`)
	t.Cleanup(func() {
		dbfx.Exec(t, `DROP TRIGGER IF EXISTS pcx_fail_cancel ON agent_task_queue`)
		dbfx.Exec(t, `DROP FUNCTION IF EXISTS pcx_fail_cancel()`)
	})
	f.put(t, testUserID, title(f.a2), nil).Want(http.StatusInternalServerError)
	if got, want := f.stored(t, testUserID), any(title(f.a1)); !equalJSON(got, want) {
		t.Fatalf("stored = %v after a failed cancel, want %v", got, want)
	}
	if got := len(f.activity(t)); got != acts {
		t.Fatalf("activity rows %d -> %d: the activity row survived the rollback", acts, got)
	}
	f.wantStatus(t, "task after a failed cancel", task, "queued")
}

func TestPersonalWriteResponseFollowsCancelCommit(t *testing.T) {
	f := memberBGFixture(t, "pcx-order")
	f.setWorkspace(t, title(f.a1))
	task := f.enqueueFor(t, testUserID, f.a1, bgagents.DMTitle, bgagents.WorkspaceSource)
	f.put(t, testUserID, title(f.a2), nil).Want(http.StatusOK)
	// Read on a separate pool connection: the cancel is committed, not pending.
	f.wantStatus(t, "task when the write response returned", task, "cancelled")
}

func TestEnqueueRefusesAChainThatChangedUnderIt(t *testing.T) {
	f := memberBGFixture(t, "pcx-stale")
	f.setWorkspace(t, title(f.a1))
	// The enqueuer resolved the workspace chain (a1); A's personal write
	// lands before the insert. The insert must not escape the cancel.
	f.put(t, testUserID, title(f.a2), nil).Want(http.StatusOK)
	stale, err := testHandler.TaskService.EnqueueBackgroundTask(context.Background(), service.BackgroundTaskInput{
		Job: bgagents.DMTitle, WorkspaceID: util.MustParseUUID(f.ws), AgentID: util.MustParseUUID(f.a1),
		MemberID: util.MustParseUUID(testUserID), Source: bgagents.WorkspaceSource, Slot: bgagents.Primary, Input: "x",
	})
	if err == nil {
		dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", util.UUIDToString(stale.ID))
		t.Fatalf("enqueued a task under a superseded chain: %v", stale.ID)
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestEnqueueWaitsForAnInFlightPersonalWrite(t *testing.T) {
	f := memberBGFixture(t, "pcx-lock")
	f.setWorkspace(t, title(f.a1))
	ctx := t.Context()
	// An in-flight write: holds the member lock and has not committed yet.
	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := service.LockMemberBackgroundAgents(ctx, tx, util.MustParseUUID(f.ws), util.MustParseUUID(testUserID)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		task, err := testHandler.TaskService.EnqueueBackgroundTask(ctx, service.BackgroundTaskInput{
			Job: bgagents.DMTitle, WorkspaceID: util.MustParseUUID(f.ws), AgentID: util.MustParseUUID(f.a1),
			MemberID: util.MustParseUUID(testUserID), Source: bgagents.WorkspaceSource, Slot: bgagents.Primary, Input: "x",
		})
		if err == nil {
			dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", util.UUIDToString(task.ID))
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("enqueue did not wait for the write's lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `INSERT INTO member_setting (workspace_id, user_id, key, value) VALUES ($1,$2,'background_agents',$3)`,
		util.MustParseUUID(f.ws), util.MustParseUUID(testUserID), []byte(mustJSON(t, title(f.a2)))); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, service.ErrBackgroundChainChanged) {
		t.Fatalf("enqueue after the write committed = %v, want ErrBackgroundChainChanged", err)
	}
}
