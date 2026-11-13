package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func seedParkedInput(test *testing.T, issueID, agentID, event string) {
	test.Helper()
	dbfx.Exec(test, `INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload, state, park_reason, thread_key)
		VALUES ($1, $2, $3, $4, 'comment', '{}', 'parked', 'failed', '00000000-0000-0000-0000-000000000000')`, testWorkspaceID, issueID, agentID, event)
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM run_input WHERE issue_id = $1`, issueID) })
}

func TestRunInputsListAndDiscard(test *testing.T) {
	fixture := newTriageRunFixture(test)
	issueID := fixture.issueFor(test, "", "parked inputs")
	seedParkedInput(test, issueID, fixture.agentID, "evt-1")
	seedParkedInput(test, issueID, fixture.agentID, "evt-2")

	var listed struct {
		Inputs []RunInputResponse `json:"inputs"`
	}
	testutil.Call(test, testHandler.ListIssueRunInputs, withURLParam(
		newRequest(http.MethodGet, "/api/issues/"+issueID+"/inputs", nil), "id", issueID)).Want(http.StatusOK).JSON(&listed)
	if len(listed.Inputs) != 2 || listed.Inputs[0].EventID != "evt-1" || listed.Inputs[0].State != "parked" {
		test.Fatalf("listed = %+v, want two parked inputs in arrival order", listed.Inputs)
	}

	var out map[string]int
	testutil.Call(test, testHandler.DiscardIssueRunInputs, withURLParam(
		newRequest(http.MethodPost, "/api/issues/"+issueID+"/inputs/discard", nil), "id", issueID)).Want(http.StatusOK).JSON(&out)
	if out["count"] != 2 {
		test.Fatalf("discarded %d, want 2", out["count"])
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM run_input WHERE issue_id = $1 AND state = 'discarded'`, issueID); n != 2 {
		test.Fatalf("%d discarded rows, want 2", n)
	}
}

func TestRunInputsResumeQueuesRunWithSameEventIDs(test *testing.T) {
	fixture := newTriageRunFixture(test)
	issueID := fixture.issueFor(test, "", "resume inputs")
	seedParkedInput(test, issueID, fixture.agentID, "evt-a")

	var out map[string]int
	testutil.Call(test, testHandler.ResumeIssueRunInputs, withURLParam(
		newRequest(http.MethodPost, "/api/issues/"+issueID+"/inputs/resume", nil), "id", issueID)).Want(http.StatusOK).JSON(&out)
	if out["count"] != 1 {
		test.Fatalf("resumed %d, want 1", out["count"])
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM run_input WHERE issue_id = $1 AND event_id = 'evt-a' AND state = 'owned' AND redelivery_of_task_id IS NULL`, issueID); n != 1 {
		test.Fatalf("resumed input is not held with its event id: %d", n)
	}
}

func TestRunInputsRejectAgentCallerAndUninvocableAgent(test *testing.T) {
	fixture := newTriageRunFixture(test)
	issueID := fixture.issueFor(test, "", "denied inputs")
	privateAgent := dbfx.Agent(test, "private inputs agent", fixture.runtimeID, testutil.Cols{"visibility": "private"})
	seedParkedInput(test, issueID, privateAgent, "evt-p")
	req := withURLParam(newRequestAs("00000000-0000-0000-0000-00000000dead", http.MethodPost, "/api/issues/"+issueID+"/inputs/discard",
		map[string]string{"agent_id": privateAgent}), "id", issueID)
	rec := testutil.Call(test, testHandler.DiscardIssueRunInputs, req)
	if rec.Code == http.StatusOK {
		test.Fatalf("a caller who may not run the agent discarded its inputs: %s", rec.Body.String())
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM run_input WHERE issue_id = $1 AND state = 'parked'`, issueID); n != 1 {
		test.Fatalf("denied caller changed the batch: %d parked", n)
	}
}

func TestSetProjectWIPLimit(test *testing.T) {
	projectID := inactiveProject(test, "in_progress")
	put := func(body any) *testutil.Response {
		return testutil.Call(test, testHandler.SetProjectWIPLimit, withURLParam(
			newRequest(http.MethodPut, "/api/projects/"+projectID+"/wip-limit", body), "id", projectID))
	}
	put(map[string]any{"limit": 4}).Want(http.StatusOK)
	if n := dbfx.Count(test, `SELECT count(*) FROM project WHERE id = $1 AND (settings->>'wip_limit')::int = 4`, projectID); n != 1 {
		test.Fatal("wip_limit 4 not stored in project settings")
	}
	put(map[string]any{"limit": 0}).Want(http.StatusBadRequest)
	put(map[string]any{"limit": nil}).Want(http.StatusOK)
	if n := dbfx.Count(test, `SELECT count(*) FROM project WHERE id = $1 AND settings ? 'wip_limit'`, projectID); n != 0 {
		test.Fatal("null limit did not clear wip_limit")
	}
}
