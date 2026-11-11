package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

var ciCard = feedCaller{source: "pat", scopes: []string{DecisionCIRequestScope}, owner: true}

// ciCardCall is a CI job's request: a task-less PAT, an issue, the owner, an ask.
func ciCardCall(test *testing.T, caller feedCaller, fixture singleCallFixture, mutate func(map[string]any)) *httptest.ResponseRecorder {
	test.Helper()
	body := map[string]any{"issue_id": fixture.issueID, "for": testUserID, "ask": fixture.details, "candidate_sha": ciSHA(1), "candidate_at": ciAt(1)}
	if mutate != nil {
		mutate(body)
	}
	response := httptest.NewRecorder()
	testHandler.CreateCIDecision(response, caller.request(test, http.MethodPost, "/api/decisions/ci", mustJSON(test, body)))
	return response
}

// ciSHA and ciAt give candidate n a 40-hex SHA and a commit time n minutes in.
func ciSHA(n int) string { return fmt.Sprintf("%040x", n) }

func ciAt(n int) string {
	return time.Date(2026, 11, 11, 9, n, 0, 0, time.UTC).Format(time.RFC3339)
}

func ciCardFor(test *testing.T, fixture singleCallFixture, n int) *httptest.ResponseRecorder {
	test.Helper()
	return ciCardCall(test, ciCard, fixture, func(body map[string]any) {
		body["candidate_sha"], body["candidate_at"] = ciSHA(n), ciAt(n)
	})
}

func ciCardID(test *testing.T, response *httptest.ResponseRecorder) string {
	test.Helper()
	var card map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &card); err != nil {
		test.Fatal(err)
	}
	id, _ := card["id"].(string)
	return id
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
	request := ciCard.request(test, http.MethodPost, "/api/decisions/ci", mustJSON(test, map[string]any{"issue_id": fixture.issueID, "for": testUserID, "ask": fixture.details, "candidate_sha": ciSHA(1), "candidate_at": ciAt(1)}))
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

func TestCIDecisionSameSHATwiceReturnsTheSameCard(test *testing.T) {
	fixture := newSingleCallFixture(test)
	first := ciCardFor(test, fixture, 1)
	if first.Code != http.StatusCreated {
		test.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	second := ciCardFor(test, fixture, 1)
	if second.Code != http.StatusOK {
		test.Fatalf("second status = %d, want 200 body=%s", second.Code, second.Body.String())
	}
	if ciCardID(test, first) == "" || ciCardID(test, first) != ciCardID(test, second) {
		test.Fatalf("second call returned card %q, want %q", ciCardID(test, second), ciCardID(test, first))
	}
	if got := ciCardCount(test, fixture); got != 1 {
		test.Fatalf("decision requests = %d, want 1", got)
	}
}

func TestCIDecisionNewerSHAWithdrawsTheOlderAsk(test *testing.T) {
	fixture := newSingleCallFixture(test)
	older := ciCardFor(test, fixture, 1)
	newer := ciCardFor(test, fixture, 2)
	if older.Code != http.StatusCreated || newer.Code != http.StatusCreated {
		test.Fatalf("statuses = %d, %d; want 201, 201 body=%s", older.Code, newer.Code, newer.Body.String())
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND status = 'open'`, fixture.issueID); got != 1 {
		test.Fatalf("open cards = %d, want 1", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'agent_withdrawn'`, ciCardID(test, older)); got != 1 {
		test.Fatalf("older card is not withdrawn")
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, ciCardID(test, newer)); got != 1 {
		test.Fatalf("newer card is not open")
	}
}

func TestCIDecisionOlderSHAAfterNewerDoesNotReplaceIt(test *testing.T) {
	fixture := newSingleCallFixture(test)
	newer := ciCardFor(test, fixture, 2)
	older := ciCardFor(test, fixture, 1)
	if older.Code != http.StatusConflict {
		test.Fatalf("older status = %d, want 409 body=%s", older.Code, older.Body.String())
	}
	if got := ciCardCount(test, fixture); got != 1 {
		test.Fatalf("decision requests = %d, want 1", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, ciCardID(test, newer)); got != 1 {
		test.Fatalf("newer card is no longer open")
	}
}

func TestCIDecisionNeedsACandidate(test *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"no sha":    func(body map[string]any) { delete(body, "candidate_sha") },
		"not a sha": func(body map[string]any) { body["candidate_sha"] = "main" },
		"no time":   func(body map[string]any) { delete(body, "candidate_at") },
		"bad time":  func(body map[string]any) { body["candidate_at"] = "yesterday" },
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newSingleCallFixture(test)
			if got := ciCardCall(test, ciCard, fixture, mutate).Code; got != http.StatusBadRequest {
				test.Fatalf("status = %d, want 400", got)
			}
			if got := ciCardCount(test, fixture); got != 0 {
				test.Fatalf("refused call wrote %d requests", got)
			}
		})
	}
}

func TestCIDecisionConcurrentCandidatesLeaveOneOpenCard(test *testing.T) {
	fixture := newSingleCallFixture(test)
	var wait sync.WaitGroup
	for n := 1; n <= 4; n++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ciCardFor(test, fixture, n)
		}()
	}
	wait.Wait()
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND status = 'open'`, fixture.issueID); got != 1 {
		test.Fatalf("open cards = %d, want 1", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND status = 'open' AND request_fingerprint LIKE $2`, fixture.issueID, "%:"+ciAt(4)); got != 1 {
		test.Fatalf("the open card is not the newest candidate")
	}
}
