package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9125: an agent never changes a project's status directly. It proposes
// project.status.set and the owner approves it with a passkey. A member keeps
// changing the status directly.

var projectStatusValues = []string{"planned", "in_progress", "paused", "completed", "cancelled"}

func projectStatusOf(test *testing.T, projectID string) string {
	test.Helper()
	var status string
	dbfx.QueryRow(test, `SELECT status FROM project WHERE id = $1`, projectID).Scan(&status)
	return status
}

func putProject(test *testing.T, projectID string, body map[string]any, decorate func(*http.Request) *http.Request) *testutil.Response {
	test.Helper()
	request := withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID, body), "id", projectID)
	if decorate != nil {
		request = decorate(request)
	}
	return testutil.Call(test, testHandler.UpdateProject, request)
}

// The CHE-9125 incident: an agent paused an active project and the server
// accepted it.
func TestAgentPausingAnActiveProjectIsRefused(test *testing.T) {
	fixture := newTriageRunFixture(test)
	taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "status": "running"})
	asAgent := func(request *http.Request) *http.Request { return asAgentActor(request, fixture.agentID, taskID) }

	for _, from := range []string{"in_progress", "planned"} {
		for _, status := range projectStatusValues {
			test.Run(from+" to "+status, func(test *testing.T) {
				projectID := inactiveProject(test, from)
				response := putProject(test, projectID, map[string]any{"status": status}, asAgent).Want(http.StatusForbidden)
				if !strings.Contains(response.Text(), "project.status.set") {
					test.Errorf("refusal %q does not name the project.status.set card", response.Text())
				}
				if got := projectStatusOf(test, projectID); got != from {
					test.Errorf("status = %s after a refused write, want %s", got, from)
				}
			})
		}
	}
}

// A paused project already refuses an agent write as project_inactive. Setting
// the status back to in_progress is a status change like any other.
func TestAgentResumingAPausedProjectIsRefused(test *testing.T) {
	fixture := newTriageRunFixture(test)
	taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "status": "running"})
	projectID := inactiveProject(test, "paused")
	putProject(test, projectID, map[string]any{"status": "in_progress"}, func(request *http.Request) *http.Request {
		return asAgentActor(request, fixture.agentID, taskID)
	}).Want(http.StatusForbidden)
	if got := projectStatusOf(test, projectID); got != "paused" {
		test.Fatalf("status = %s, want paused", got)
	}
}

func TestMachineCredentialCannotSetProjectStatus(test *testing.T) {
	projectID := inactiveProject(test, "in_progress")
	response := putProject(test, projectID, map[string]any{"status": "paused"}, asCloudNodeActor).Want(http.StatusForbidden)
	if !strings.Contains(response.Text(), "project.status.set") {
		test.Errorf("refusal %q does not name the project.status.set card", response.Text())
	}
	if got := projectStatusOf(test, projectID); got != "in_progress" {
		test.Fatalf("status = %s, want in_progress", got)
	}
}

func TestAgentStillEditsOtherProjectFields(test *testing.T) {
	fixture := newTriageRunFixture(test)
	taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"runtime_id": fixture.runtimeID, "status": "running"})
	projectID := inactiveProject(test, "in_progress")
	putProject(test, projectID, map[string]any{"title": "Renamed by an agent", "priority": "high"}, func(request *http.Request) *http.Request {
		return asAgentActor(request, fixture.agentID, taskID)
	}).Want(http.StatusOK)
	if got := projectStatusOf(test, projectID); got != "in_progress" {
		test.Fatalf("status = %s, want in_progress", got)
	}
}

func TestMemberSetsProjectStatusDirectly(test *testing.T) {
	for _, status := range projectStatusValues {
		test.Run(status, func(test *testing.T) {
			projectID := inactiveProject(test, "in_progress")
			putProject(test, projectID, map[string]any{"status": status}, nil).Want(http.StatusOK)
			if got := projectStatusOf(test, projectID); got != status {
				test.Fatalf("status = %s, want %s", got, status)
			}
		})
	}
}
