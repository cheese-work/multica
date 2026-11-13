package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// askFixture is a running task whose issue was requested by a workspace member:
// decision_ask derives issue, agent and recipient from the task, not the body.
func newAskFixture(test *testing.T) singleCallFixture {
	test.Helper()
	fixture := newSingleCallFixture(test)
	dbfx.Exec(test, `UPDATE issue SET requested_by_id = $1 WHERE id = $2`, fixture.humanID, fixture.issueID)
	return fixture
}

func (fixture singleCallFixture) askBody() map[string]any {
	return map[string]any{"ask": fixture.details, "footer": fixture.footer, "escalation_class": "reversible"}
}

func (fixture singleCallFixture) askCall(body map[string]any) *http.Request {
	request := fixture.call(body)
	request.URL.Path = "/api/decisions/ask"
	return request
}

func TestDecisionAskOpensRequestOnTaskIssueWithServerPickedLane(test *testing.T) {
	fixture := newAskFixture(test)
	var created map[string]any
	testutil.Call(test, testHandler.AskDecision, fixture.askCall(fixture.askBody())).Want(http.StatusCreated).JSON(&created)
	if created["issue_id"] != fixture.issueID || created["human_user_id"] != fixture.humanID || created["asking_agent_id"] != fixture.agentID {
		test.Fatalf("request = issue %v human %v agent %v, want the task's issue, requester and agent", created["issue_id"], created["human_user_id"], created["asking_agent_id"])
	}
	if requests := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); requests != 1 {
		test.Fatalf("decision requests = %d, want 1", requests)
	}
	if lanes := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1 AND decision_topic_id IS NOT NULL`, fixture.humanID); lanes != 1 {
		test.Fatalf("decision lanes = %d, want 1", lanes)
	}
}

func TestDecisionAskRefusesAMemberCredential(test *testing.T) {
	fixture := newAskFixture(test)
	request := fixture.askCall(fixture.askBody())
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	testutil.Call(test, testHandler.AskDecision, request).Want(http.StatusForbidden)
	if requests := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); requests != 0 {
		test.Fatalf("refused call wrote %d requests", requests)
	}
}

func TestDecisionAskCannotTargetAnotherIssue(test *testing.T) {
	fixture := newAskFixture(test)
	other := dbfx.Issue(test, "other issue", testutil.Cols{"requested_by_id": fixture.humanID})
	body := fixture.askBody()
	body["issue_id"] = other
	testutil.Call(test, testHandler.AskDecision, fixture.askCall(body)).Want(http.StatusBadRequest)
	if requests := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id IN ($1, $2)`, fixture.issueID, other); requests != 0 {
		test.Fatalf("refused call wrote %d requests", requests)
	}
}

func TestDecisionAskWithoutAPersonRefusesInsteadOfGuessing(test *testing.T) {
	fixture := newSingleCallFixture(test) // no requester; the creator is the test user, not an agent-reachable pick
	dbfx.Exec(test, `UPDATE issue SET creator_type = 'agent', creator_id = $1 WHERE id = $2`, fixture.agentID, fixture.issueID)
	testutil.Call(test, testHandler.AskDecision, fixture.askCall(fixture.askBody())).Want(http.StatusConflict)
	if requests := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); requests != 0 {
		test.Fatalf("refused call wrote %d requests", requests)
	}
}
