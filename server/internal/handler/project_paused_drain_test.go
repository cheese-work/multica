package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// An ordinary issue update on a paused project drains like the pause itself:
// the running run lives on, every run that did not start is cancelled.
// Cancelled and completed projects still stop every run.

func pausedDrainTaskStatus(test *testing.T, taskID string) string {
	test.Helper()
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		test.Fatal(err)
	}
	return task.Status
}

// F4 REQUIRED_TEST: a run keeps running through a title write and a status
// write of its own, its next comment returns 201, and nothing new is enqueued.
func TestPausedProjectRunningRunSurvivesOwnIssueWrites(test *testing.T) {
	fixture := newTriageRunFixture(test)
	projectID := inactiveProject(test, "in_progress")
	issueID := fixture.issueFor(test, "", "paused drain own writes")
	dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
	runningTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID, "status": "running"})
	testutil.Call(test, testHandler.UpdateProject, withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID,
		map[string]any{"status": "paused"}), "id", projectID)).Want(http.StatusOK)
	tasksBefore := tasksOn(test, issueID)

	write := func(handler http.HandlerFunc, method, path string, body map[string]any) *testutil.Response {
		request := withURLParam(newRequest(method, path, body), "id", issueID)
		return testutil.Call(test, handler, asAgentActor(request, fixture.agentID, runningTask))
	}
	write(testHandler.UpdateIssue, http.MethodPut, "/api/issues/"+issueID, map[string]any{"title": "Retitled by its own run", "suppress_run": true}).Want(http.StatusOK)
	if got := pausedDrainTaskStatus(test, runningTask); got != "running" {
		test.Fatalf("title write changed the running run to %s", got)
	}
	write(testHandler.UpdateIssue, http.MethodPut, "/api/issues/"+issueID, map[string]any{"status": "in_review", "suppress_run": true}).Want(http.StatusOK)
	if got := pausedDrainTaskStatus(test, runningTask); got != "running" {
		test.Fatalf("status write changed the running run to %s", got)
	}
	write(testHandler.CreateComment, http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "Next comment of the same run"}).Want(http.StatusCreated)
	if got := tasksOn(test, issueID); got != tasksBefore {
		test.Errorf("task count on the issue changed from %d to %d", tasksBefore, got)
	}
}

// A member write on a paused project cancels only the runs that did not start.
func TestPausedProjectIssueUpdateCancelsOnlyUnstartedRuns(test *testing.T) {
	fixture := newTriageRunFixture(test)
	projectID := inactiveProject(test, "paused")
	issueID := fixture.issueFor(test, "", "paused drain unstarted")
	dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
	tasks := map[string]string{}
	for _, runStatus := range []string{"queued", "dispatched", "running", "waiting_local_directory", "deferred"} {
		tasks[runStatus] = dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID,
			"status": runStatus, "trigger_comment_id": dbfx.Comment(test, issueID, runStatus)})
	}
	testutil.Call(test, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID,
		map[string]any{"title": "Member retitle", "suppress_run": true}), "id", issueID)).Want(http.StatusOK)
	for runStatus, taskID := range tasks {
		want := "cancelled"
		if runStatus == "running" {
			want = "running"
		}
		if got := pausedDrainTaskStatus(test, taskID); got != want {
			test.Errorf("%s run is %s after an update on a paused project, want %s", runStatus, got, want)
		}
	}
}

// Closed projects keep stopping every run, a running one included.
func TestClosedProjectIssueUpdateCancelsRunningRun(test *testing.T) {
	fixture := newTriageRunFixture(test)
	for _, status := range []string{"cancelled", "completed"} {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, status)
			issueID := fixture.issueFor(test, "", "closed drain")
			dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
			runningTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID, "status": "running"})
			testutil.Call(test, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID,
				map[string]any{"title": "Member retitle", "suppress_run": true}), "id", issueID)).Want(http.StatusOK)
			if got := pausedDrainTaskStatus(test, runningTask); got != "cancelled" {
				test.Errorf("update on a %s project left the running run %s", status, got)
			}
		})
	}
}
