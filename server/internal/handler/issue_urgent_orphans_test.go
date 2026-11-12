package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// An urgent issue is an orphan when nothing is going to pick it up: no
// queued/running task and no enabled wakeup. The pulse reads this list.
func TestListUrgentOrphanIssues(t *testing.T) {
	runtime := dbfx.Runtime(t, "urgent orphan runtime")
	agent := dbfx.Agent(t, "urgent orphan agent", runtime)

	urgent := func(title string, over ...testutil.Cols) string {
		cols := testutil.Cols{"priority": "urgent"}
		for _, o := range over {
			for k, v := range o {
				cols[k] = v
			}
		}
		return dbfx.Issue(t, title, cols)
	}
	wakeup := func(issue string, enabled bool) {
		dbfx.Insert(t, "issue_wakeup", testutil.Cols{
			"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": issue,
			"agent_id": agent, "created_by": testUserID, "instruction": "resume", "kind": "at", "mode": "once",
			"next_fire_at": testutil.Raw("now() + interval '1 hour'"), "enabled": enabled,
		})
	}
	task := func(issue, status string) {
		dbfx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "status": status})
	}

	orphan := urgent("orphan")
	disabledWakeup := urgent("disabled wakeup")
	wakeup(disabledWakeup, false)
	finishedTask := urgent("finished task")
	task(finishedTask, "completed")

	running := urgent("running")
	task(running, "running")
	queued := urgent("queued")
	task(queued, "queued")
	waiting := urgent("enabled wakeup")
	wakeup(waiting, true)
	done := urgent("done", testutil.Cols{"status": "done"})
	high := dbfx.Issue(t, "high orphan", testutil.Cols{"priority": "high"})

	var resp struct {
		Issues []struct {
			ID string `json:"id"`
		} `json:"issues"`
	}
	testutil.Call(t, testHandler.ListUrgentOrphanIssues, newRequest("GET", "/api/issues/urgent-orphans", nil)).
		Want(http.StatusOK).JSON(&resp)

	got := map[string]bool{}
	for _, i := range resp.Issues {
		got[i.ID] = true
	}
	for _, id := range []string{orphan, disabledWakeup, finishedTask} {
		if !got[id] {
			t.Errorf("issue %s should be listed as an urgent orphan", id)
		}
	}
	for name, id := range map[string]string{"running": running, "queued": queued, "enabled wakeup": waiting, "done": done, "high": high} {
		if got[id] {
			t.Errorf("%s issue %s must not be listed", name, id)
		}
	}
}
