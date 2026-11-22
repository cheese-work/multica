package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// taskEvent writes the block event a refused operation of the task leaves for a
// member on an issue (CHE-8752: the run complies by asking, never naming it).
func taskEvent(t *testing.T, issueID, taskID, member string) string {
	t.Helper()
	id := dbid.NewV7()
	dbfx.Exec(t, `INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode, member_id) VALUES ($1, $2, $3, $4, 'comment', $5, 'block', $6)`,
		id, testWorkspaceID, issueID, taskID, uuid.NewString(), member)
	dbfx.Cleanup(t, `DELETE FROM ask_guard_event WHERE id = $1`, id)
	return uuidToString(id)
}

func recoveryEventIDs(t *testing.T, issueID string) map[string]bool {
	t.Helper()
	events, err := testHandler.Queries.ListAskRecoveryEvents(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]bool{}
	for _, event := range events {
		if uuidToString(event.IssueID) == issueID {
			open[uuidToString(event.ID)] = true
		}
	}
	return open
}

// TestOwnRequestServesTheRunsOpenEventsForThatPerson: a run whose operation was
// refused asks the same person on the same issue with a decision request. The
// request serves the run's open events for that person, so the recovery pass
// finds nothing to repair and no second card follows.
func TestOwnRequestServesTheRunsOpenEventsForThatPerson(t *testing.T) {
	setAskGuardMode(t, "block")
	fixture := newSingleCallFixture(t)
	first := taskEvent(t, fixture.issueID, fixture.taskID, fixture.humanID)
	second := taskEvent(t, fixture.issueID, fixture.taskID, fixture.humanID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	if open := recoveryEventIDs(t, fixture.issueID); !open[first] || !open[second] {
		t.Fatalf("setup: events open for recovery = %v", open)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, fixture.taskID)

	testutil.Call(t, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated)

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	if open := recoveryEventIDs(t, fixture.issueID); len(open) != 0 {
		t.Fatalf("events still open for recovery after the run's own request: %v", open)
	}
	if !eventCleared(t, first) || !eventCleared(t, second) {
		t.Fatalf("cleared first=%v second=%v, want both", eventCleared(t, first), eventCleared(t, second))
	}
}

// TestOwnRequestLeavesOtherPeopleIssuesAndRunsOpen: only the run's events for
// the person it asked, on the issue it asked on, are served.
func TestOwnRequestLeavesOtherPeopleIssuesAndRunsOpen(t *testing.T) {
	setAskGuardMode(t, "block")
	fixture := newSingleCallFixture(t)
	other := dbfx.User(t, "Other human", "own-request-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "member"})
	otherIssue := dbfx.Issue(t, "other issue", nil)
	forOther := taskEvent(t, fixture.issueID, fixture.taskID, other)
	elsewhere := taskEvent(t, otherIssue, fixture.taskID, fixture.humanID)
	otherRun := taskEvent(t, fixture.issueID, uuid.NewString(), fixture.humanID)
	served := taskEvent(t, fixture.issueID, fixture.taskID, fixture.humanID)

	testutil.Call(t, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated)

	for name, id := range map[string]string{"another person": forOther, "another issue": elsewhere, "another run": otherRun} {
		if eventCleared(t, id) {
			t.Errorf("the request cleared an event for %s", name)
		}
	}
	if !eventCleared(t, served) {
		t.Error("the request left the run's own event for that person open")
	}
}

// TestRecoversRefusedWhenTheRunAlreadyAskedThePerson: a repair run that names an
// event whose source run already opened a request to that person on that issue
// is refused with the existing request id and opens no second card, even when
// the question differs and the fingerprint dedupe cannot catch it.
func TestRecoversRefusedWhenTheRunAlreadyAskedThePerson(t *testing.T) {
	setAskGuardMode(t, "block")
	fixture := newSingleCallFixture(t)
	var created map[string]any
	testutil.Call(t, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated).JSON(&created)
	event := taskEvent(t, fixture.issueID, fixture.taskID, fixture.humanID)

	again := fixture.details
	again.Decision = "A different question, so the request fingerprint differs?"
	body := fixture.body()
	body["ask"] = again
	body["recovers"] = event
	var refusal struct {
		Error string `json:"error"`
	}
	testutil.Call(t, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusConflict).JSON(&refusal)
	if id, _ := created["id"].(string); !strings.Contains(refusal.Error, id) {
		t.Fatalf("refusal %q does not name the existing request %s", refusal.Error, id)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); n != 1 {
		t.Fatalf("requests on the issue = %d, want 1", n)
	}
}
