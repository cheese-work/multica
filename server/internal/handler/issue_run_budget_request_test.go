package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type budgetRequestResponse struct {
	RequestID  string `json:"request_id"`
	ActionID   string `json:"action_id"`
	ApproveURL string `json:"approve_url"`
	Requests   []struct {
		RequestID  string `json:"request_id"`
		ActionID   string `json:"action_id"`
		ApproveURL string `json:"approve_url"`
	} `json:"requests"`
}

// budgetRequestFixture is a budgeted issue and an owner who can approve.
type budgetRequestFixture struct {
	singleCallFixture
	window string
}

func newBudgetRequestFixture(test *testing.T) budgetRequestFixture {
	test.Helper()
	fixture := newActionFixture(test)
	budgetFixtureIssue(test, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM decision_action WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM run_budget_refusal WHERE issue_id = $1`, fixture.issueID)
	return budgetRequestFixture{fixture, windowID(test, fixture.issueID)}
}

func (fixture budgetRequestFixture) request(test *testing.T, user, source string, body map[string]any) *testutil.Response {
	test.Helper()
	request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+fixture.issueID+"/run-budget/requests", body), "id", fixture.issueID)
	request.Header.Set("X-User-ID", user)
	request.Header.Set("X-Actor-Source", source)
	// The route mounts RequireHumanActor; the tests go through it too.
	return testutil.Call(test, RequireHumanActor(http.HandlerFunc(testHandler.RequestIssueRunBudgetAction)).ServeHTTP, request)
}

func (fixture budgetRequestFixture) openRequests(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND request_fingerprint LIKE 'server:run_budget_action:%'`, fixture.issueID)
}

func TestRequestRunBudgetRaiseOpensASpendRequestForAnOwner(test *testing.T) {
	fixture := newBudgetRequestFixture(test)
	refusal := uuid.NewString()
	dbfx.Exec(test, `INSERT INTO run_budget_refusal (id, workspace_id, issue_id, window_id) VALUES ($1, $2, $3, $4)`, refusal, testWorkspaceID, fixture.issueID, fixture.window)
	var got budgetRequestResponse
	fixture.request(test, fixture.humanID, "pat", map[string]any{"kind": "raise", "new_cap": 9, "window_id": fixture.window}).Want(http.StatusCreated).JSON(&got)
	if got.RequestID == "" || got.ActionID == "" || len(got.Requests) != 1 {
		test.Fatalf("response = %+v", got)
	}
	if want := "/" + handlerTestWorkspaceSlug + "/issues/" + fixture.issueID + "?approve=" + got.ActionID; got.ApproveURL != want {
		test.Fatalf("approve_url = %q, want %q", got.ApproveURL, want)
	}
	var fingerprint, class, origin, human, params, actionType string
	dbfx.QueryRow(test, `SELECT r.request_fingerprint, r.escalation_class, r.origin, r.human_user_id::text, a.params::text, a.action_type FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id WHERE r.id = $1 AND a.id = $2`, got.RequestID, got.ActionID).
		Scan(&fingerprint, &class, &origin, &human, &params, &actionType)
	if want := "server:run_budget_action:raise:" + fixture.window + ":9"; fingerprint != want {
		test.Fatalf("fingerprint = %q, want %q", fingerprint, want)
	}
	if class != "spend" || human != fixture.humanID || actionType != "issue.run_budget.raise" {
		test.Fatalf("class=%s human=%s type=%s", class, human, actionType)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(params), &decoded); err != nil || decoded["refusal_id"] != refusal || decoded["new_cap"] != float64(9) {
		test.Fatalf("params = %s (%v); want refusal_id %s", params, err, refusal)
	}
	if fixture.openRequests(test) != 1 {
		test.Fatalf("requests = %d, want 1", fixture.openRequests(test))
	}
}

func TestRequestRunBudgetTopUpLeavesRefusalEmptyWithoutOne(test *testing.T) {
	fixture := newBudgetRequestFixture(test)
	var got budgetRequestResponse
	fixture.request(test, fixture.humanID, "interactive_jwt", map[string]any{"kind": "top_up", "window_id": fixture.window}).Want(http.StatusCreated).JSON(&got)
	var fingerprint, params string
	dbfx.QueryRow(test, `SELECT r.request_fingerprint, a.params::text FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id WHERE r.id = $1`, got.RequestID).Scan(&fingerprint, &params)
	if want := "server:run_budget_action:top_up:" + fixture.window + ":0"; fingerprint != want {
		test.Fatalf("fingerprint = %q, want %q", fingerprint, want)
	}
	if strings.Contains(params, "refusal_id") {
		test.Fatalf("params %s name a refusal; none is open", params)
	}
}

func TestRequestRunBudgetRepeatReturnsTheOpenRequest(test *testing.T) {
	fixture := newBudgetRequestFixture(test)
	body := map[string]any{"kind": "raise", "new_cap": 9, "window_id": fixture.window}
	var first, second budgetRequestResponse
	fixture.request(test, fixture.humanID, "pat", body).Want(http.StatusCreated).JSON(&first)
	fixture.request(test, fixture.humanID, "pat", body).Want(http.StatusOK).JSON(&second)
	if first.RequestID != second.RequestID || first.ActionID != second.ActionID || first.ApproveURL != second.ApproveURL {
		test.Fatalf("repeat = %+v, first = %+v", second, first)
	}
	if fixture.openRequests(test) != 1 {
		test.Fatalf("requests = %d, want 1", fixture.openRequests(test))
	}
	body["new_cap"] = 10
	var other budgetRequestResponse
	fixture.request(test, fixture.humanID, "pat", body).Want(http.StatusCreated).JSON(&other)
	if other.RequestID == first.RequestID {
		test.Fatal("a different cap reused the first request")
	}
}

func TestRequestRunBudgetFromANonOwnerAsksEveryOwner(test *testing.T) {
	fixture := newBudgetRequestFixture(test)
	member := dbfx.User(test, "Plain member", "plain-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": member, "role": "member"})
	owners := dbfx.Count(test, `SELECT count(*) FROM member WHERE workspace_id = $1 AND role = 'owner'`, testWorkspaceID)
	var got budgetRequestResponse
	fixture.request(test, member, "pat", map[string]any{"kind": "top_up", "window_id": fixture.window}).Want(http.StatusCreated).JSON(&got)
	if len(got.Requests) != owners || fixture.openRequests(test) != owners {
		test.Fatalf("responses = %d, requests = %d; want one per owner (%d)", len(got.Requests), fixture.openRequests(test), owners)
	}
	if asked := dbfx.Count(test, `SELECT count(DISTINCT human_user_id) FROM decision_request WHERE issue_id = $1 AND human_user_id = $2`, fixture.issueID, member); asked != 0 {
		test.Fatal("the non-owner caller was asked to approve")
	}
}

func TestRequestRunBudgetRefusals(test *testing.T) {
	fixture := newBudgetRequestFixture(test)
	valid := func() map[string]any {
		return map[string]any{"kind": "raise", "new_cap": 9, "window_id": fixture.window}
	}
	cases := map[string]struct {
		source string
		edit   func(map[string]any)
		status int
	}{
		"task token":       {"task_token", func(map[string]any) {}, http.StatusForbidden},
		"cloud PAT":        {"cloud_pat", func(map[string]any) {}, http.StatusForbidden},
		"unknown kind":     {"pat", func(b map[string]any) { b["kind"] = "refund" }, http.StatusBadRequest},
		"raise needs cap":  {"pat", func(b map[string]any) { delete(b, "new_cap") }, http.StatusBadRequest},
		"top-up has a cap": {"pat", func(b map[string]any) { b["kind"] = "top_up" }, http.StatusBadRequest},
		"bad window":       {"pat", func(b map[string]any) { b["window_id"] = "yesterday" }, http.StatusBadRequest},
	}
	for name, c := range cases {
		test.Run(name, func(test *testing.T) {
			body := valid()
			c.edit(body)
			fixture.request(test, fixture.humanID, c.source, body).Want(c.status)
		})
	}
	if fixture.openRequests(test) != 0 {
		test.Fatalf("a refused call opened %d requests", fixture.openRequests(test))
	}
}

func TestRequestRunBudgetFromAPATCannotBeApprovedByThatPAT(test *testing.T) {
	fixture := newBudgetRequestFixture(test)
	var got budgetRequestResponse
	fixture.request(test, fixture.humanID, "pat", map[string]any{"kind": "top_up", "window_id": fixture.window}).Want(http.StatusCreated).JSON(&got)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, got.RequestID)
	action := pendingAction{fixture.singleCallFixture, got.RequestID, got.ActionID}
	body, err := json.Marshal(map[string]any{"payload_sha256": action.hash(test), "assertion": map[string]any{"id": "not-a-passkey"}})
	if err != nil {
		test.Fatal(err)
	}
	for _, handler := range []*Handler{testHandler, passkeyHandler(test)} {
		request := action.decide("approve", action.humanID, "pat")
		request.Body = readerCloser{bytes.NewReader(body)}
		if response := testutil.Call(test, handler.ApproveDecisionAction, request); response.Code < 400 {
			test.Fatalf("a PAT approval returned %d: %s", response.Code, response.Text())
		}
	}
	action.assertUntouched(test)
}

func (fixture budgetRequestFixture) readAction(test *testing.T, user, actionID string) *testutil.Response {
	test.Helper()
	request := withURLParam(newRequest(http.MethodGet, "/api/decision-actions/"+actionID, nil), "id", actionID)
	request.Header.Set("X-User-ID", user)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	return testutil.Call(test, testHandler.GetDecisionAction, request)
}

// The approve URL must reach a view that can approve: it names the action, and
// the action is readable by the owner who is asked, and by nobody else.
func TestRequestRunBudgetApproveURLNamesTheActionTheOwnerCanRead(test *testing.T) {
	fixture := newBudgetRequestFixture(test)
	var got budgetRequestResponse
	fixture.request(test, fixture.humanID, "pat", map[string]any{"kind": "raise", "new_cap": 9, "window_id": fixture.window}).Want(http.StatusCreated).JSON(&got)
	if want := "/" + handlerTestWorkspaceSlug + "/issues/" + fixture.issueID + "?approve=" + got.ActionID; got.ApproveURL != want {
		test.Fatalf("approve_url = %q, want %q", got.ApproveURL, want)
	}
	var action struct {
		ID            string         `json:"id"`
		ActionType    string         `json:"action_type"`
		PayloadSHA256 string         `json:"payload_sha256"`
		State         string         `json:"state"`
		Params        map[string]any `json:"params"`
	}
	fixture.readAction(test, fixture.humanID, got.ActionID).Want(http.StatusOK).JSON(&action)
	if action.ID != got.ActionID || action.ActionType != "issue.run_budget.raise" || len(action.PayloadSHA256) != 64 || action.State != "pending" || action.Params["new_cap"] != float64(9) {
		test.Fatalf("action = %+v", action)
	}
	other := dbfx.User(test, "Other member", "other-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "member"})
	fixture.readAction(test, other, got.ActionID).Want(http.StatusForbidden)
}

// An action card lives in a chat session. The read names it, so the approve page
// can link to the card when it cannot decide the action itself.
func TestGetDecisionActionNamesTheChatSessionOfItsCard(test *testing.T) {
	fixture := newBudgetRequestFixture(test)
	var got budgetRequestResponse
	fixture.request(test, fixture.humanID, "pat", map[string]any{"kind": "raise", "new_cap": 9, "window_id": fixture.window}).Want(http.StatusCreated).JSON(&got)
	var view struct {
		ChatSessionID string `json:"chat_session_id"`
	}
	fixture.readAction(test, fixture.humanID, got.ActionID).Want(http.StatusOK).JSON(&view)
	var stored string
	dbfx.QueryRow(test, `SELECT chat_session_id::text FROM decision_request WHERE id = $1`, got.RequestID).Scan(&stored)
	if stored == "" || view.ChatSessionID != stored {
		test.Fatalf("chat_session_id = %q, want the card session %q", view.ChatSessionID, stored)
	}
}
