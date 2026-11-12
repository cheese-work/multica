package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-5118: agent_task_queue.priority is stamped at enqueue. Raising an issue
// to urgent afterwards must move its queued/deferred tasks ahead of older
// medium work; tasks that already started keep their rank.
func TestIssuePriorityChangeReranksPendingTasks(t *testing.T) {
	f := newClaimDecisionFixture(t, 1)
	ctx := context.Background()
	agent := util.UUIDToString(f.agentID)
	runtime := util.UUIDToString(f.runtimeID)

	older := f.fx.Issue(t, "older medium", testutil.Cols{"priority": "medium"})
	urgentLater := f.fx.Issue(t, "raised later", testutil.Cols{"priority": "medium"})
	running := f.fx.Issue(t, "already running", testutil.Cols{"priority": "medium"})
	mk := func(issue, status string) string {
		return f.fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "status": status, "priority": 2})
	}
	olderTask := mk(older, "queued")
	queuedTask := mk(urgentLater, "queued")
	deferredTask := f.fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": urgentLater, "status": "deferred", "priority": 2, "fire_at": testutil.Raw("now() + interval '1 hour'")})
	runningTask := mk(running, "running")

	for _, id := range []string{urgentLater, running} {
		if _, err := f.pool.Exec(ctx, `UPDATE issue SET priority = 'urgent' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	prio := func(id string) int {
		var p int
		f.fx.QueryRow(t, `SELECT priority FROM agent_task_queue WHERE id = $1`, id).Scan(&p)
		return p
	}
	if got := prio(queuedTask); got != 4 {
		t.Errorf("queued task priority = %d, want 4", got)
	}
	if got := prio(deferredTask); got != 4 {
		t.Errorf("deferred task priority = %d, want 4", got)
	}
	if got := prio(runningTask); got != 2 {
		t.Errorf("running task priority = %d, want 2 (unchanged)", got)
	}
	if got := prio(olderTask); got != 2 {
		t.Errorf("unrelated task priority = %d, want 2", got)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, runningTask); err != nil {
		t.Fatal(err)
	}

	claimed, err := f.svc.ClaimTaskForRuntime(ctx, f.runtimeID)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimTaskForRuntime = %v, %v", claimed, err)
	}
	if got := util.UUIDToString(claimed.ID); got != queuedTask {
		t.Errorf("claimed %s, want the raised task %s ahead of older medium %s", got, queuedTask, olderTask)
	}
}
