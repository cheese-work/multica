package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func inactiveProject(test *testing.T, status string) string {
	test.Helper()
	return dbfx.Project(test, "Project admission", testutil.Cols{"status": status})
}

func TestInactiveProjectRequestTriggers(test *testing.T) {
	fixture := newTriageRunFixture(test)
	for _, status := range []string{"planned", "in_progress", "paused", "cancelled", "completed"} {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, status)
			for _, entry := range triageEntryPoints() {
				test.Run(entry.name, func(test *testing.T) {
					issueID := fixture.issueFor(test, "", "project trigger: "+entry.name)
					dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
					response := entry.call(test, fixture, issueID)
					active := status == "planned" || status == "in_progress"
					if active {
						response.WantOneOf(http.StatusOK, http.StatusCreated, http.StatusAccepted)
						if tasksOn(test, issueID) == 0 {
							test.Fatal("active project did not start a run")
						}
					} else {
						if tasksOn(test, issueID) != 0 {
							test.Errorf("%s started a run in a %s project", entry.name, status)
						}
						if entry.name == "manual rerun" {
							response.Want(http.StatusForbidden)
							if response.Map()["reason_code"] != "project_inactive" {
								test.Errorf("missing rerun project_inactive refusal: %s", response.Text())
							}
							return
						}
						response.WantOneOf(http.StatusOK, http.StatusCreated)
						var body struct {
							TriggerOutcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
						}
						response.JSON(&body)
						if len(body.TriggerOutcomes) != 1 || body.TriggerOutcomes[0].ReasonCode != ReasonProjectInactive || body.TriggerOutcomes[0].Status != DispatchBlocked {
							test.Errorf("missing blocked/project_inactive outcome: %s", response.Text())
						}
					}
				})
			}
		})
	}
}

func TestInactiveProjectAgentWrites(test *testing.T) {
	fixture := newTriageRunFixture(test)
	actorTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "status": "running"})
	for _, status := range []string{"paused", "cancelled", "completed"} {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, status)
			issueID := fixture.issueFor(test, "", "inactive agent write")
			dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
			cases := []struct {
				name    string
				handler http.HandlerFunc
				method  string
				body    map[string]any
			}{
				{"create", testHandler.CreateIssue, http.MethodPost, map[string]any{"title": "Forbidden new issue", "project_id": projectID, "status": "backlog"}},
				{"edit", testHandler.UpdateIssue, http.MethodPut, map[string]any{"title": "Forbidden edit"}},
				{"comment", testHandler.CreateComment, http.MethodPost, map[string]any{"content": "Forbidden comment"}},
				{"status", testHandler.UpdateIssue, http.MethodPut, map[string]any{"status": "in_progress"}},
				{"assign", testHandler.UpdateIssue, http.MethodPut, map[string]any{"assignee_type": "agent", "assignee_id": fixture.leaderID}},
			}
			for _, testCase := range cases {
				if testCase.name == "create" && status == "paused" {
					continue // intake stays open on a paused project; see TestPausedProjectAgentCreate
				}
				test.Run(testCase.name, func(test *testing.T) {
					request := withURLParam(newRequest(testCase.method, "/api/issues/"+issueID, testCase.body), "id", issueID)
					request = asAgentActor(request, fixture.agentID, actorTask)
					response := testutil.Call(test, testCase.handler, request)
					response.Want(http.StatusForbidden)
					if response.Map()["reason_code"] != "project_inactive" {
						test.Errorf("missing project_inactive: %s", response.Text())
					}
				})
			}
			issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
			if err != nil {
				test.Fatal(err)
			}
			if issue.Title != "inactive agent write" || issue.Status != "todo" || uuidToString(issue.AssigneeID) != fixture.agentID || commentsOn(test, issueID) != 0 {
				test.Errorf("refused writes mutated issue: %+v", issue)
			}
			if dbfx.Count(test, "SELECT count(*) FROM issue WHERE project_id=$1", projectID) != 1 {
				test.Error("agent created an issue in the inactive project")
			}
		})
	}
}

func TestInactiveProjectCloseCancelsRunsAndResumeDoesNotReplay(test *testing.T) {
	fixture := newTriageRunFixture(test)
	for _, status := range []string{"paused", "cancelled", "completed"} {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, "in_progress")
			issueID := fixture.issueFor(test, "", "project close")
			dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
			var eventMu sync.Mutex
			cancelledEvents := make(map[string]int)
			testHandler.Bus.Subscribe(protocol.EventTaskCancelled, func(event events.Event) {
				if event.WorkspaceID == testWorkspaceID {
					eventMu.Lock()
					cancelledEvents[event.TaskID]++
					eventMu.Unlock()
				}
			})
			var taskIDs []string
			for _, runStatus := range []string{"queued", "dispatched", "running", "deferred"} {
				taskIDs = append(taskIDs, dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID, "status": runStatus,
					"trigger_comment_id": dbfx.Comment(test, issueID, runStatus)}))
			}
			update := func(next string) {
				testutil.Call(test, testHandler.UpdateProject, withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID,
					map[string]any{"status": next}), "id", projectID)).Want(http.StatusOK)
			}
			update(status)
			// A pause drains: the running run lives on. Cancel and complete stop every run.
			wantLive := 0
			if status == "paused" {
				wantLive = 1
			}
			if count := dbfx.Count(test, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status <> 'cancelled'", issueID); count != wantLive {
				test.Errorf("%s left %d uncancelled runs, want %d", status, count, wantLive)
			}
			eventMu.Lock()
			for index, taskID := range taskIDs {
				wantEvents := 1
				if status == "paused" && index == 2 {
					wantEvents = 0
				}
				if cancelledEvents[taskID] != wantEvents {
					test.Errorf("task %s published %d cancellation events, want %d", taskID, cancelledEvents[taskID], wantEvents)
				}
			}
			eventMu.Unlock()
			testutil.Call(test, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID,
				map[string]any{"title": "Member can still edit"}), "id", issueID)).Want(http.StatusOK)
			update("planned")
			testutil.Call(test, testHandler.CreateComment, withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments",
				map[string]any{"content": "[@Agent](mention://agent/" + fixture.agentID + ") new trigger"}), "id", issueID)).Want(http.StatusCreated)
			if tasksOn(test, issueID) != 5 {
				test.Error("new trigger after resume did not enqueue exactly one run")
			}
		})
	}
}

func TestInactiveProjectMoveCancelsRuns(test *testing.T) {
	fixture := newTriageRunFixture(test)
	for _, status := range []string{"paused", "cancelled", "completed"} {
		for _, mode := range []string{"single", "description", "batch"} {
			test.Run(status+"/"+mode, func(test *testing.T) {
				projectID := inactiveProject(test, status)
				issueID := fixture.issueFor(test, "", "move active runs")
				for _, runStatus := range []string{"queued", "dispatched", "running", "waiting_local_directory", "deferred"} {
					dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID,
						"status": runStatus, "trigger_comment_id": dbfx.Comment(test, issueID, runStatus)})
				}
				body := map[string]any{"project_id": projectID, "suppress_run": true}
				if mode == "description" {
					body["description"] = "Member move"
				}
				if mode == "batch" {
					testutil.Call(test, testHandler.BatchUpdateIssues, newRequest(http.MethodPut, "/api/issues/batch",
						map[string]any{"issue_ids": []string{issueID}, "updates": body})).Want(http.StatusOK)
				} else {
					testutil.Call(test, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, body), "id", issueID)).Want(http.StatusOK)
				}
				// A pause drains: only the running run survives a move into it.
				wantLive := 0
				if status == "paused" {
					wantLive = 1
				}
				if count := dbfx.Count(test, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status <> 'cancelled'", issueID); count != wantLive {
					test.Errorf("moving into %s left %d active runs, want %d", status, count, wantLive)
				}
				if status == "paused" && dbfx.Count(test, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='running'", issueID) != 1 {
					test.Error("moving into paused cancelled the running run")
				}
			})
		}
	}
}

func TestInactiveProjectStartRefusesAndSettles(test *testing.T) {
	for _, status := range []string{"paused", "cancelled", "completed"} {
		for _, mode := range []string{"legacy", "claim"} {
			test.Run(status+"/"+mode, func(test *testing.T) {
				taskID, runtimeID, generation := startClaimFixture(test, "dispatched")
				projectID := inactiveProject(test, status)
				dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=(SELECT issue_id FROM agent_task_queue WHERE id=$1)", taskID, projectID)
				request := startClaimRequest(taskID, runtimeID, generation)
				if mode == "legacy" {
					request = withURLParam(newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/start", map[string]any{}), "taskId", taskID)
				}
				response := testutil.Call(test, testHandler.StartTask, request)
				if response.Code != http.StatusForbidden {
					test.Errorf("inactive project start status = %d, want 403: %s", response.Code, response.Text())
				} else if response.Map()["reason_code"] != "project_inactive" {
					test.Errorf("start refusal lacks project_inactive: %s", response.Text())
				}
				task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
				if err != nil {
					test.Fatal(err)
				}
				if task.Status != "cancelled" || task.Error.String != "project_inactive" || task.FailureReason.String != "project_inactive" {
					test.Errorf("refused start not settled: status=%s error=%s reason=%s", task.Status, task.Error.String, task.FailureReason.String)
				}
			})
		}
	}
}

func TestInactiveProjectAgentBatchDelete(test *testing.T) {
	fixture := newTriageRunFixture(test)
	actorTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "status": "running"})
	for _, status := range []string{"paused", "cancelled", "completed"} {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, status)
			activeIssue := fixture.issueFor(test, "", "active batch peer")
			issueID := fixture.issueFor(test, "", "inactive batch delete")
			dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
			request := newRequest(http.MethodPost, "/api/issues/batch-delete", map[string]any{"issue_ids": []string{activeIssue, issueID}})
			response := testutil.Call(test, testHandler.BatchDeleteIssues, asAgentActor(request, fixture.agentID, actorTask))
			response.Want(http.StatusForbidden)
			if response.Map()["reason_code"] != "project_inactive" {
				test.Errorf("batch delete refusal lacks project_inactive: %s", response.Text())
			}
			if count := dbfx.Count(test, "SELECT count(*) FROM issue WHERE id IN ($1,$2)", activeIssue, issueID); count != 2 {
				test.Errorf("refused batch deleted issues: %d remain", count)
			}
		})
	}
}

func TestInactiveProjectStaleStartCannotCancel(test *testing.T) {
	taskID, runtimeID, generation := startClaimFixture(test, "dispatched")
	projectID := inactiveProject(test, "paused")
	dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=(SELECT issue_id FROM agent_task_queue WHERE id=$1)", taskID, projectID)
	testutil.Call(test, testHandler.StartTask, startClaimRequest(taskID, runtimeID, generation.Add(-time.Microsecond))).Want(http.StatusConflict)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		test.Fatal(err)
	}
	if task.Status != "dispatched" || task.Error.Valid {
		test.Errorf("stale start mutated another claim: status=%s error=%s", task.Status, task.Error.String)
	}
}

func TestInactiveProjectCloseSerializesMoveOut(test *testing.T) {
	for _, destination := range []string{"project", "none"} {
		test.Run(destination, func(test *testing.T) {
			source := inactiveProject(test, "in_progress")
			target := ""
			var projectValue any
			if destination == "project" {
				target = inactiveProject(test, "in_progress")
				projectValue = target
			}
			issueID := dbfx.Issue(test, "move out during project close", testutil.Cols{"project_id": source})
			response := raceAgainst(test, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), "SELECT id FROM project WHERE id=$1 FOR UPDATE", source); err != nil {
					test.Fatal(err)
				}
			}, func() *testutil.Response {
				return testutil.Call(test, testHandler.UpdateIssue, withURLParam(withActorSource(newRequest(http.MethodPut, "/api/issues/"+issueID,
					map[string]any{"project_id": projectValue, "suppress_run": true}), "interactive_jwt"), "id", issueID))
			})
			response.Want(http.StatusOK)
			if got := issueProject(test, issueID); got != target {
				test.Errorf("project = %q, want %q", got, target)
			}
		})
	}
}

func TestInactiveProjectAutomaticRetry(test *testing.T) {
	fixture := newTriageRunFixture(test)
	projectID := inactiveProject(test, "paused")
	issueID := fixture.issueFor(test, "", "inactive retry")
	dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
	taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID,
		"status": "failed", "failure_reason": "timeout", "attempt": 1, "max_attempts": 2})
	parent, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		test.Fatal(err)
	}
	child, _ := testHandler.TaskService.MaybeRetryFailedTask(context.Background(), parent)
	if child != nil || tasksOn(test, issueID) != 1 {
		test.Fatal("automatic retry created a run for an inactive project")
	}
}

func TestInactiveProjectWriteTriggerDecision(test *testing.T) {
	fixture := newTriageRunFixture(test)
	for _, source := range []string{"assignment", "status"} {
		test.Run(source, func(test *testing.T) {
			projectID := inactiveProject(test, "paused")
			issueID := fixture.issueFor(test, "", "inactive "+source)
			dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
			issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
			if err != nil {
				test.Fatal(err)
			}
			input := service.IssueTriggerInput{Issue: issue, IsCreate: source == "assignment", StatusChanged: source == "status", PrevStatus: "backlog"}
			if _, admitted := testHandler.IssueService.WillEnqueueRun(context.Background(), input, service.IssueTriggerProbe{}); admitted {
				test.Error("the shared write admission predicate admitted an inactive project")
			}
			dbfx.Exec(test, "UPDATE project SET status='in_progress' WHERE id=$1", projectID)
			if _, admitted := testHandler.IssueService.WillEnqueueRun(context.Background(), input, service.IssueTriggerProbe{}); !admitted {
				test.Error("the shared write admission predicate refused the active-project control")
			}
		})
	}
}

func TestInactiveProjectMemberWriteTriggers(test *testing.T) {
	fixture := newTriageRunFixture(test)
	for _, status := range []string{"paused", "cancelled", "completed"} {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, status)
			var created IssueResponse
			testutil.Call(test, testHandler.CreateIssue, newRequest(http.MethodPost, "/api/issues", map[string]any{
				"title": "Member-created inactive issue", "project_id": projectID,
				"assignee_type": "agent", "assignee_id": fixture.agentID,
			})).Want(http.StatusCreated).JSON(&created)
			dbfx.Cleanup(test, "DELETE FROM issue WHERE id=$1", created.ID)
			dbfx.Cleanup(test, "DELETE FROM activity_log WHERE issue_id=$1", created.ID)
			if tasksOn(test, created.ID) != 0 || len(created.TriggerOutcomes) != 1 || created.TriggerOutcomes[0].ReasonCode != ReasonProjectInactive {
				test.Fatalf("member creation did not report project_inactive: %+v", created.TriggerOutcomes)
			}
			issueID := fixture.issueFor(test, "", "inactive status trigger")
			dbfx.Exec(test, "UPDATE issue SET project_id=$2,status='backlog' WHERE id=$1", issueID, projectID)
			var updated IssueResponse
			testutil.Call(test, testHandler.UpdateIssue, withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID,
				map[string]any{"status": "in_progress"}), "id", issueID)).Want(http.StatusOK).JSON(&updated)
			if tasksOn(test, issueID) != 0 || len(updated.TriggerOutcomes) != 1 || updated.TriggerOutcomes[0].ReasonCode != ReasonProjectInactive {
				test.Fatalf("member status change did not report project_inactive: %+v", updated.TriggerOutcomes)
			}
			batchIssueID := fixture.issueFor(test, "", "inactive batch status trigger")
			dbfx.Exec(test, "UPDATE issue SET project_id=$2,status='backlog' WHERE id=$1", batchIssueID, projectID)
			var batch struct {
				Updated         int                     `json:"updated"`
				TriggerOutcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
			}
			testutil.Call(test, testHandler.BatchUpdateIssues, newRequest(http.MethodPut, "/api/issues/batch", map[string]any{
				"issue_ids": []string{batchIssueID}, "updates": map[string]any{"status": "in_progress"},
			})).Want(http.StatusOK).JSON(&batch)
			if batch.Updated != 1 || tasksOn(test, batchIssueID) != 0 || len(batch.TriggerOutcomes) != 1 || batch.TriggerOutcomes[0].ReasonCode != ReasonProjectInactive {
				test.Fatalf("member batch did not report project_inactive: %+v", batch)
			}
		})
	}
}

func TestPausedProjectLetsRunningRunWrite(test *testing.T) {
	fixture := newTriageRunFixture(test)
	projectID := inactiveProject(test, "in_progress")
	issueID := fixture.issueFor(test, "", "paused project drain")
	dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
	otherIssueID := fixture.issueFor(test, "", "paused project drain, other project")
	runningTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID, "status": "running"})
	queuedTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID, "status": "queued",
		"trigger_comment_id": dbfx.Comment(test, issueID, "queued")})
	foreignTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": otherIssueID, "status": "running"})
	testutil.Call(test, testHandler.UpdateProject, withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID,
		map[string]any{"status": "paused"}), "id", projectID)).Want(http.StatusOK)
	statusOf := func(taskID string) string {
		task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
		if err != nil {
			test.Fatal(err)
		}
		return task.Status
	}
	if got := statusOf(runningTask); got != "running" {
		test.Errorf("pause changed the running run to %s", got)
	}
	if got := statusOf(queuedTask); got != "cancelled" {
		test.Errorf("pause left the queued run %s", got)
	}
	call := func(taskID string, handler http.HandlerFunc, method string, body map[string]any) *testutil.Response {
		request := withURLParam(newRequest(method, "/api/issues/"+issueID, body), "id", issueID)
		return testutil.Call(test, handler, asAgentActor(request, fixture.agentID, taskID))
	}
	call(runningTask, testHandler.CreateComment, http.MethodPost, map[string]any{"content": "Final report from a draining run"}).Want(http.StatusCreated)
	call(runningTask, testHandler.UpdateIssue, http.MethodPut, map[string]any{"title": "Updated by a draining run", "suppress_run": true}).Want(http.StatusOK)
	// A run of another project, or one that already ended, is still refused.
	for name, taskID := range map[string]string{"another project": foreignTask, "cancelled run": queuedTask} {
		response := call(taskID, testHandler.CreateComment, http.MethodPost, map[string]any{"content": "Refused comment"})
		response.Want(http.StatusForbidden)
		if response.Map()["reason_code"] != "project_inactive" {
			test.Errorf("%s: missing project_inactive: %s", name, response.Text())
		}
	}
	// A paused project still takes new issues from an agent (CHE-10021).
	create := newRequest(http.MethodPost, "/api/issues", map[string]any{"title": "New issue in paused project", "project_id": projectID, "status": "backlog"})
	testutil.Call(test, testHandler.CreateIssue, asAgentActor(create, fixture.agentID, runningTask)).Want(http.StatusCreated)
	if commentsOn(test, issueID) != 2 {
		test.Errorf("want the queued trigger comment and the drain comment, got %d", commentsOn(test, issueID))
	}
}

func TestCancelledProjectRefusesRunningRunWrite(test *testing.T) {
	fixture := newTriageRunFixture(test)
	for _, status := range []string{"cancelled", "completed"} {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, "in_progress")
			issueID := fixture.issueFor(test, "", "closed project write")
			dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
			runningTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "issue_id": issueID, "status": "running"})
			testutil.Call(test, testHandler.UpdateProject, withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID,
				map[string]any{"status": status}), "id", projectID)).Want(http.StatusOK)
			if got := dbfx.Count(test, "SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='cancelled'", runningTask); got != 1 {
				test.Fatalf("%s did not cancel the running run", status)
			}
			request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "Refused"}), "id", issueID)
			response := testutil.Call(test, testHandler.CreateComment, asAgentActor(request, fixture.agentID, runningTask))
			response.Want(http.StatusForbidden)
			if response.Map()["reason_code"] != "project_inactive" {
				test.Errorf("missing project_inactive: %s", response.Text())
			}
		})
	}
}

// A paused project stops new runs only: an agent can still create issues in it,
// and the assigned agent's run is blocked, not started. (CHE-10021)
func TestPausedProjectAgentCreate(test *testing.T) {
	fixture := newTriageRunFixture(test)
	actorTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "status": "running"})
	projectID := inactiveProject(test, "paused")
	create := newRequest(http.MethodPost, "/api/issues", map[string]any{
		"title": "Assigned issue in paused project", "project_id": projectID, "status": "todo",
		"assignee_type": "agent", "assignee_id": fixture.agentID,
	})
	response := testutil.Call(test, testHandler.CreateIssue, asAgentActor(create, fixture.agentID, actorTask))
	response.Want(http.StatusCreated)
	var body struct {
		ID              string                  `json:"id"`
		TriggerOutcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
	}
	response.JSON(&body)
	if tasksOn(test, body.ID) != 0 {
		test.Error("agent create started a run in a paused project")
	}
	if len(body.TriggerOutcomes) != 1 || body.TriggerOutcomes[0].ReasonCode != ReasonProjectInactive || body.TriggerOutcomes[0].Status != DispatchBlocked {
		test.Errorf("want one blocked/project_inactive outcome: %s", response.Text())
	}
}

// A refused agent write names the project status and the refused action in
// `message`; `error` and `reason_code` keep their value. Cancelled and
// completed projects still refuse intake. (CHE-10021)
func TestInactiveProjectRefusalNamesAction(test *testing.T) {
	fixture := newTriageRunFixture(test)
	actorTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "status": "running"})
	for _, status := range []string{"cancelled", "completed"} {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, status)
			create := newRequest(http.MethodPost, "/api/issues", map[string]any{"title": "Refused create", "project_id": projectID, "status": "backlog"})
			response := testutil.Call(test, testHandler.CreateIssue, asAgentActor(create, fixture.agentID, actorTask))
			response.Want(http.StatusForbidden)
			fields := response.Map()
			if fields["error"] != "project_inactive" || fields["reason_code"] != "project_inactive" {
				test.Errorf("error/reason_code changed: %s", response.Text())
			}
			message, _ := fields["message"].(string)
			if !strings.Contains(message, status) || !strings.Contains(message, "create issues") {
				test.Errorf("message must name the status %q and the refused action: %s", status, response.Text())
			}
			if dbfx.Count(test, "SELECT count(*) FROM issue WHERE project_id=$1", projectID) != 0 {
				test.Error("refused create left an issue")
			}
		})
	}
	// A comment on an issue of a paused project by a run that is not running is refused with its own action.
	projectID := inactiveProject(test, "paused")
	issueID := fixture.issueFor(test, "", "refused comment")
	dbfx.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issueID, projectID)
	request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "Refused"}), "id", issueID)
	response := testutil.Call(test, testHandler.CreateComment, asAgentActor(request, fixture.agentID, actorTask))
	response.Want(http.StatusForbidden)
	if message, _ := response.Map()["message"].(string); !strings.Contains(message, "paused") || !strings.Contains(message, "write") {
		test.Errorf("write refusal message missing status or action: %s", response.Text())
	}
}
