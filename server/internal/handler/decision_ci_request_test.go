package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

var ciCard = feedCaller{source: "pat", scopes: []string{DecisionCIRequestScope}, owner: true}

// ciCardCall is a CI job's request: a task-less PAT, an issue, the owner, an ask.
func ciCardCall(test *testing.T, caller feedCaller, fixture singleCallFixture, mutate func(map[string]any)) *httptest.ResponseRecorder {
	test.Helper()
	body := map[string]any{"issue_id": fixture.issueID, "for": testUserID, "ask": fixture.details}
	if mutate != nil {
		mutate(body)
	}
	response := httptest.NewRecorder()
	testHandler.CreateCIDecision(response, caller.request(test, http.MethodPost, "/api/decisions/ci", mustJSON(test, body)))
	return response
}

func ciCardCount(test *testing.T, fixture singleCallFixture) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID)
}

func TestCIDecisionScopedPATCreatesCard(test *testing.T) {
	fixture := newSingleCallFixture(test)
	var created map[string]any
	response := ciCardCall(test, ciCard, fixture, nil)
	if response.Code != http.StatusCreated {
		test.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		test.Fatal(err)
	}
	if created["issue_id"] != fixture.issueID || created["human_user_id"] != testUserID || created["escalation_class"] != "deploy" {
		test.Fatalf("card = issue %v human %v class %v, want the named issue, owner and class deploy", created["issue_id"], created["human_user_id"], created["escalation_class"])
	}
	if got := ciCardCount(test, fixture); got != 1 {
		test.Fatalf("decision requests = %d, want 1", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, fixture.issueID); got != 0 {
		test.Fatalf("decision actions = %d, want 0", got)
	}
}

func TestCIDecisionRefusesEveryOtherCredential(test *testing.T) {
	for name, caller := range map[string]feedCaller{
		"no scope":          {source: "pat", owner: true},
		"feed scope only":   {source: "pat", scopes: []string{DecisionFeedScope}, owner: true},
		"non-owner":         {source: "pat", scopes: []string{DecisionCIRequestScope}, owner: false},
		"member credential": {source: "interactive_jwt", scopes: []string{DecisionCIRequestScope}, owner: true},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newSingleCallFixture(test)
			if got := ciCardCall(test, caller, fixture, nil).Code; got != http.StatusForbidden {
				test.Fatalf("status = %d, want 403", got)
			}
			if got := ciCardCount(test, fixture); got != 0 {
				test.Fatalf("refused call wrote %d requests", got)
			}
		})
	}
}

func TestCIDecisionRefusesAWrongUserToken(test *testing.T) {
	fixture := newSingleCallFixture(test)
	request := ciCard.request(test, http.MethodPost, "/api/decisions/ci", mustJSON(test, map[string]any{"issue_id": fixture.issueID, "for": testUserID, "ask": fixture.details}))
	request.Header.Set("X-User-ID", fixture.humanID) // the header names someone the token does not belong to
	response := httptest.NewRecorder()
	testHandler.CreateCIDecision(response, request)
	if response.Code != http.StatusForbidden {
		test.Fatalf("status = %d, want 403", response.Code)
	}
	if got := ciCardCount(test, fixture); got != 0 {
		test.Fatalf("refused call wrote %d requests", got)
	}
}

func TestCIDecisionBodyCannotWidenTheCard(test *testing.T) {
	fixture := newSingleCallFixture(test)
	response := ciCardCall(test, ciCard, fixture, func(body map[string]any) {
		body["escalation_class"] = "reversible"
		body["action"] = map[string]any{"type": "noop"}
		body["covers_request_ids"] = []string{"00000000-0000-0000-0000-000000000001"}
	})
	if response.Code != http.StatusCreated {
		test.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND escalation_class = 'deploy'`, fixture.issueID); got != 1 {
		test.Fatalf("deploy cards = %d, want 1: the body must not set the class", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, fixture.issueID); got != 0 {
		test.Fatalf("decision actions = %d, want 0", got)
	}
}
