package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Personal writes cancel the member's tasks (CHE-4114).

// fixedTarget resolves to a slot chosen before the enqueue: a stale resolution.
func fixedTarget(agent string, src bgagents.Source) service.BackgroundResolver {
	return func(context.Context) (service.BackgroundTarget, error) {
		return service.BackgroundTarget{AgentID: util.MustParseUUID(agent), Source: src, Slot: bgagents.Primary}, nil
	}
}

// liveTarget resolves the member's chain as production does, first slot wins.
func liveTarget(ws string, then func(bgagents.Resolution)) service.BackgroundResolver {
	return func(ctx context.Context) (service.BackgroundTarget, error) {
		r, err := testHandler.ResolveBackgroundAgents(ctx, util.MustParseUUID(ws), util.MustParseUUID(testUserID), bgagents.DMTitle)
		if err != nil || len(r.Chain) == 0 {
			return service.BackgroundTarget{}, errors.Join(err, errors.New("no chain"))
		}
		then(r)
		return service.BackgroundTarget{AgentID: util.MustParseUUID(r.Chain[0].AgentID), Source: r.Source, Slot: r.Chain[0].Slot}, nil
	}
}

func (f memberBG) enqueueFor(t *testing.T, member, agent string, job bgagents.Job, src bgagents.Source) db.AgentTaskQueue {
	t.Helper()
	task, err := testHandler.TaskService.EnqueueBackgroundTask(context.Background(), service.BackgroundTaskInput{
		Job: job, WorkspaceID: util.MustParseUUID(f.ws),
		MemberID: util.MustParseUUID(member), Input: "hello", Resolve: fixedTarget(agent, src),
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
		Job: bgagents.DMTitle, WorkspaceID: util.MustParseUUID(f.ws),
		MemberID: util.MustParseUUID(testUserID), Input: "x", Resolve: fixedTarget(f.a1, bgagents.WorkspaceSource),
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

// enqueueBehindInFlightWrite starts an enqueue while an uncommitted personal
// write holds the member lock, runs inWrite inside that write, commits it, and
// returns what the resolver saw and the enqueue's result. The resolver must
// run only after the commit.
func (f memberBG) enqueueBehindInFlightWrite(t *testing.T, inWrite func(ctx context.Context, tx pgx.Tx)) (bgagents.Resolution, db.AgentTaskQueue, error) {
	t.Helper()
	ctx := t.Context()
	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := service.LockMemberBackgroundAgents(ctx, tx, util.MustParseUUID(f.ws), util.MustParseUUID(testUserID)); err != nil {
		t.Fatal(err)
	}
	var committed atomic.Bool
	var seen bgagents.Resolution
	type result struct {
		task db.AgentTaskQueue
		err  error
	}
	done := make(chan result, 1)
	go func() {
		task, err := testHandler.TaskService.EnqueueBackgroundTask(ctx, service.BackgroundTaskInput{
			Job: bgagents.DMTitle, WorkspaceID: util.MustParseUUID(f.ws), MemberID: util.MustParseUUID(testUserID), Input: "x",
			Resolve: liveTarget(f.ws, func(r bgagents.Resolution) {
				if !committed.Load() {
					t.Error("the chain was resolved before the in-flight write committed")
				}
				seen = r
			}),
		})
		done <- result{task, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("enqueue did not wait for the write's lock: %v", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	inWrite(ctx, tx)
	committed.Store(true)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err == nil {
		dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", util.UUIDToString(r.task.ID))
	}
	return seen, r.task, r.err
}

func (f memberBG) setUserIn(t *testing.T, ctx context.Context, tx pgx.Tx, value map[string]any) {
	t.Helper()
	if _, err := tx.Exec(ctx, `UPDATE member_setting SET value=$3 WHERE workspace_id=$1 AND user_id=$2 AND key='background_agents'`,
		util.MustParseUUID(f.ws), util.MustParseUUID(testUserID), []byte(mustJSON(t, value))); err != nil {
		t.Fatal(err)
	}
}

// A fallback-only write commits while an enqueue waits: the enqueue resolves
// after it, so it sees the new chain instead of inserting under a stale one.
func TestEnqueueResolvesAfterAnInFlightFallbackWrite(t *testing.T) {
	f := memberBGFixture(t, "pcx-lock")
	f.setUser(t, testUserID, title(f.a1))
	seen, task, err := f.enqueueBehindInFlightWrite(t, func(ctx context.Context, tx pgx.Tx) {
		f.setUserIn(t, ctx, tx, map[string]any{"dm_title": map[string]any{"agent_id": f.a1, "fallback_agent_id": f.a2}})
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(seen.Chain) != 2 || seen.Chain[1].AgentID != f.a2 {
		t.Fatalf("resolver saw %+v, want the committed fallback %s", seen.Chain, f.a2)
	}
	f.wantStatus(t, "task enqueued after the write", task, "queued")
}

// A -> B -> A commits while an enqueue waits: the old A task is cancelled and
// stays so; the enqueue resolves after the round trip and gets a new task.
func TestEnqueueResolvesAfterAnInFlightRoundTrip(t *testing.T) {
	f := memberBGFixture(t, "pcx-rt")
	f.setUser(t, testUserID, title(f.a1))
	old := f.enqueueFor(t, testUserID, f.a1, bgagents.DMTitle, bgagents.UserSource)
	_, task, err := f.enqueueBehindInFlightWrite(t, func(ctx context.Context, tx pgx.Tx) {
		f.setUserIn(t, ctx, tx, title(f.a2))
		if _, err := service.CancelBackgroundTasksForMember(ctx, testHandler.Queries.WithTx(tx), util.MustParseUUID(f.ws), util.MustParseUUID(testUserID), string(bgagents.DMTitle)); err != nil {
			t.Fatal(err)
		}
		f.setUserIn(t, ctx, tx, title(f.a1))
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	f.wantStatus(t, "task queued under the first A", old, "cancelled")
	f.wantStatus(t, "task enqueued after the round trip", task, "queued")
	if task.ID == old.ID {
		t.Fatal("the enqueue reused the cancelled task")
	}
}

// A workspace write that arrives after the resolver read the workspace chain
// waits for the enqueue to commit, then cancels the task it inserted. Without
// the workspace lock the write commits first and the old task escapes.
func TestWorkspaceWriteWaitsForAnInFlightEnqueue(t *testing.T) {
	f := memberBGFixture(t, "pcx-wslock")
	f.setWorkspace(t, title(f.a1))
	var write chan *testutil.Response
	var early *testutil.Response
	task, err := testHandler.TaskService.EnqueueBackgroundTask(t.Context(), service.BackgroundTaskInput{
		Job: bgagents.DMTitle, WorkspaceID: util.MustParseUUID(f.ws), MemberID: util.MustParseUUID(testUserID), Input: "x",
		Resolve: liveTarget(f.ws, func(bgagents.Resolution) {
			write = make(chan *testutil.Response, 1)
			go func() { write <- putBG(t, f.ws, title(f.a2), nil) }()
			select {
			case early = <-write:
				t.Error("the workspace write committed between the chain read and the insert")
			case <-time.After(300 * time.Millisecond):
			}
		}),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", util.UUIDToString(task.ID))
	if early == nil {
		early = <-write
	}
	early.Want(http.StatusOK)
	f.wantStatus(t, "task enqueued under the replaced workspace chain", task, "cancelled")
}
