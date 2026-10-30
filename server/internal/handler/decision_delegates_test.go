package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// placeDecisionHuman sets the human's workspace role and, when delegate is true,
// lists them in decision_delegates of a project the issue belongs to. It
// returns the project id.
func placeDecisionHuman(test *testing.T, issueID, humanID, role string, delegate bool) string {
	test.Helper()
	dbfx.Exec(test, `UPDATE member SET role = $1 WHERE workspace_id = $2 AND user_id = $3`, role, testWorkspaceID, humanID)
	projectID := dbfx.Project(test, "Decision delegates")
	if delegate {
		dbfx.Exec(test, `UPDATE project SET decision_delegates = ARRAY[$1::uuid] WHERE id = $2`, humanID, projectID)
	}
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, projectID, issueID)
	return projectID
}

func (fixture requestCoreFixture) placeHuman(test *testing.T, role string, delegate bool) requestCoreFixture {
	test.Helper()
	placeDecisionHuman(test, fixture.issueID, fixture.humanID, role, delegate)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err != nil {
		test.Fatal(err)
	}
	fixture.issue = issue
	return fixture
}

func requireAuthorityRefusal(test *testing.T, err error, status int) {
	test.Helper()
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || refusal.status != status || !strings.Contains(refusal.message, "class_needs_owner") {
		test.Fatalf("want %d class_needs_owner refusal, got %v", status, err)
	}
}

func TestDecisionDelegateTag(test *testing.T) {
	for _, tc := range []struct {
		name     string
		role     string
		delegate bool
		want     int
	}{
		{"owner without delegates", "owner", false, http.StatusCreated},
		{"delegate", "member", true, http.StatusCreated},
		{"non-delegate", "member", false, http.StatusBadRequest},
		{"admin delegate", "admin", true, http.StatusCreated},
		{"admin non-delegate", "admin", false, http.StatusBadRequest},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newIntentFixture(test)
			placeDecisionHuman(test, fixture.issueID, fixture.humanID, tc.role, tc.delegate)
			fixture.post(test, "task_token", fixture.taskID, fixture.mention(), nil).Want(tc.want)
			wantRows := 0
			if tc.want == http.StatusCreated {
				wantRows = 1
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE issue_id = $1`, fixture.issueID); count != wantRows {
				test.Fatalf("intents = %d, want %d", count, wantRows)
			}
		})
	}
}

func TestDecisionDelegatePing(test *testing.T) {
	for _, tc := range []struct {
		name     string
		role     string
		delegate bool
		class    string
		refused  bool
	}{
		{"owner spend", "owner", false, "spend", false},
		{"delegate reversible", "member", true, "reversible", false},
		{"delegate spend", "member", true, "spend", true},
		{"delegate product_direction", "member", true, "product_direction", true},
		{"non-delegate reversible", "member", false, "reversible", true},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false).placeHuman(test, tc.role, tc.delegate)
			input := fixture.input()
			input.EscalationClass = tc.class
			_, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
			if tc.refused {
				requireAuthorityRefusal(test, err, http.StatusConflict)
				fixture.assertCounts(test, 0)
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			fixture.assertCounts(test, 1)
		})
	}
}

func TestDecisionDelegateAnswerRecord(test *testing.T) {
	for _, tc := range []struct {
		name     string
		role     string
		delegate bool
		class    string
		refused  bool
	}{
		{"owner", "owner", false, "reversible", false},
		{"delegate reversible", "member", true, "reversible", false},
		{"delegate spend (class raised after the ping)", "member", true, "spend", true},
		{"non-delegate reversible", "member", false, "reversible", true},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = $1 WHERE id = $2`, tc.class, fixture.decision.ID)
			fixture.requestCoreFixture = fixture.requestCoreFixture.placeHuman(test, tc.role, tc.delegate)
			_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if tc.refused {
				requireAuthorityRefusal(test, err, http.StatusForbidden)
				fixture.assertRecordCounts(test, 0)
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			fixture.assertRecordCounts(test, 1)
		})
	}
}

func TestDecisionDelegateUnpromptedRecord(test *testing.T) {
	for _, tc := range []struct {
		name     string
		role     string
		delegate bool
		class    string
		refused  bool
	}{
		{"owner spend", "owner", false, "spend", false},
		{"delegate reversible", "member", true, "reversible", false},
		{"delegate spend", "member", true, "spend", true},
		{"delegate product_direction", "member", true, "product_direction", true},
		{"non-delegate reversible", "member", false, "reversible", true},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			fixture.requestCoreFixture = fixture.requestCoreFixture.placeHuman(test, tc.role, tc.delegate)
			input := fixture.recordInput()
			input.ParentID, input.EscalationClass = pgtype.UUID{}, tc.class
			_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
			created := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND origin = 'unprompted'`, fixture.issueID)
			if tc.refused {
				requireAuthorityRefusal(test, err, http.StatusForbidden)
				if created != 0 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, fixture.issueID) != 0 {
					test.Fatal("refused unprompted record left rows behind")
				}
				return
			}
			if err != nil || created != 1 {
				test.Fatalf("unprompted record: err=%v requests=%d", err, created)
			}
		})
	}
}

func raiseDecisionClassAs(test *testing.T, fixture askedRecordFixture, requestID pgtype.UUID, class string) *httptest.ResponseRecorder {
	test.Helper()
	request := fixture.recordRequest()
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	request.Header.Set("X-User-ID", fixture.humanID)
	request.Body = ioBody(`{"escalation_class":"` + class + `"}`)
	response := httptest.NewRecorder()
	testHandler.RaiseDecisionClass(response, withIssueParam(request, uuidToString(requestID)))
	return response
}

func TestDecisionDelegateRaiseClass(test *testing.T) {
	for _, branch := range []string{"open", "pending_answer", "woken_answer", "unprompted"} {
		test.Run(branch, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newAskedRecordFixture(test)
			fixture.requestCoreFixture = fixture.requestCoreFixture.placeHuman(test, "member", true)
			requestID := fixture.decision.ID
			var answerID pgtype.UUID
			if branch != "open" {
				input := fixture.recordInput()
				if branch == "unprompted" {
					input.ParentID = pgtype.UUID{}
					input.EscalationClass = "reversible"
				}
				record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
				if err != nil {
					test.Fatal(err)
				}
				requestID, answerID = record.Request.ID, record.Answer.ID
			}
			if branch == "woken_answer" {
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
				dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, answerID)
			}
			response := raiseDecisionClassAs(test, fixture, requestID, "spend")
			if response.Code != http.StatusOK {
				test.Fatalf("raise status=%d body=%s", response.Code, response.Body.String())
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'class_needs_owner' AND escalation_class = 'spend'`, requestID); count != 1 {
				test.Fatal("raise above reversible did not cancel the request with class_needs_owner")
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_class_change WHERE decision_request_id = $1`, requestID) != 1 {
				test.Fatal("raise left no class change record")
			}
			if branch == "open" {
				if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID) != 0 {
					test.Fatal("open request grew an answer")
				}
				return
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NOT NULL AND disclaim_reason = 'class_needs_owner' AND wake_at IS NULL OR id = $1 AND woken_at IS NOT NULL AND disclaim_reason = 'class_needs_owner'`, answerID); count != 1 {
				test.Fatal("live answer was not disclaimed with class_needs_owner")
			}
			var withdrawalID string
			dbfx.QueryRow(test, `SELECT withdrawal_comment_id FROM decision_answer WHERE id = $1`, answerID).Scan(&withdrawalID)
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			decisionPass(test, runner)
			wantRetractions := 0
			if branch == "woken_answer" {
				wantRetractions = 1
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2`, fixture.issueID, withdrawalID); count != wantRetractions {
				test.Fatalf("retraction runs = %d, want %d", count, wantRetractions)
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, requestID) != 0 {
				test.Fatal("withdrawal reopened the request")
			}
			if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID) != 1 {
				test.Fatal("raise re-pinged the human")
			}
		})
	}
}

func TestDecisionDelegateRaiseClassNonDelegate(test *testing.T) {
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	fixture.requestCoreFixture = fixture.requestCoreFixture.placeHuman(test, "member", false)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err == nil {
		test.Fatalf("non-delegate record was accepted: %+v", record)
	}
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'reversible' WHERE id = $1`, fixture.decision.ID)
	if response := raiseDecisionClassAs(test, fixture, fixture.decision.ID, "spend"); response.Code != http.StatusOK {
		test.Fatalf("raise status=%d body=%s", response.Code, response.Body.String())
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'class_needs_owner' AND escalation_class = 'spend'`, fixture.decision.ID) != 1 {
		test.Fatal("raise on a non-delegate's request did not cancel it with class_needs_owner")
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_class_change WHERE decision_request_id = $1`, fixture.decision.ID) != 1 {
		test.Fatal("raise left no class change record")
	}
}

func TestDecisionDelegateSingleCallPing(test *testing.T) {
	for _, tc := range []struct {
		name     string
		delegate bool
		want     int
	}{
		{"delegate", true, http.StatusCreated},
		{"non-delegate", false, http.StatusConflict},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newSingleCallFixture(test)
			projectID := placeDecisionHuman(test, fixture.issueID, fixture.humanID, "member", tc.delegate)
			response := testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(tc.want)
			if tc.want == http.StatusConflict {
				if !strings.Contains(response.Body.String(), "class_needs_owner") {
					test.Fatalf("refusal does not name class_needs_owner: %s", response.Body.String())
				}
				return
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND project_id = $2`, fixture.issueID, projectID) != 1 {
				test.Fatal("request did not record the issue's project")
			}
		})
	}
}

func TestDecisionDelegateRaiseClassOwnerUnchanged(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	if response := raiseDecisionClassAs(test, fixture, record.Request.ID, "deploy"); response.Code != http.StatusOK {
		test.Fatalf("raise status=%d body=%s", response.Code, response.Body.String())
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered' AND cancel_reason IS NULL AND escalation_class = 'deploy'`, record.Request.ID) != 1 ||
		dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NULL AND wake_at IS NULL`, record.Answer.ID) != 1 {
		test.Fatal("owner raise no longer holds the answer in place")
	}
}

func TestDecisionDelegateLifecycle(test *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(test *testing.T, issueID, projectID, humanID string)
		reason string
	}{
		{"delegate stays", func(*testing.T, string, string, string) {}, ""},
		{"delegate removed", func(test *testing.T, _, projectID, _ string) {
			dbfx.Exec(test, `UPDATE project SET decision_delegates = '{}' WHERE id = $1`, projectID)
		}, "delegate_removed"},
		{"issue left the project", func(test *testing.T, issueID, _, _ string) {
			dbfx.Exec(test, `UPDATE issue SET project_id = NULL WHERE id = $1`, issueID)
		}, "delegate_removed"},
		{"issue moved to another project that also delegates", func(test *testing.T, issueID, projectID, humanID string) {
			dbfx.Exec(test, `UPDATE decision_request SET project_id = $1 WHERE issue_id = $2`, projectID, issueID)
			other := dbfx.Project(test, "Other delegates")
			dbfx.Exec(test, `UPDATE project SET decision_delegates = ARRAY[$1::uuid] WHERE id = $2`, humanID, other)
			dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, other, issueID)
		}, "delegate_removed"},
		{"held class", func(test *testing.T, issueID, _, _ string) {
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE issue_id = $1`, issueID)
		}, "class_needs_owner"},
	} {
		test.Run(tc.name, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newAskedRecordFixture(test)
			projectID := placeDecisionHuman(test, fixture.issueID, fixture.humanID, "member", true)
			tc.mutate(test, fixture.issueID, projectID, fixture.humanID)
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			if tc.reason == "" {
				if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, fixture.decision.ID) != 1 {
					test.Fatal("lifecycle cancelled a request whose human is still a delegate")
				}
				return
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = $2`, fixture.decision.ID, tc.reason) != 1 {
				test.Fatalf("request not cancelled with %s", tc.reason)
			}
			if dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%decision-intent%'`, fixture.issueID) < 1 {
				test.Fatal("cancel notice does not tell the agent to post a fresh decision intent")
			}
		})
	}
}

func TestDecisionDelegateDeliveryEligibility(test *testing.T) {
	for _, delegate := range []bool{true, false} {
		name := "delegate"
		if !delegate {
			name = "non-delegate"
		}
		test.Run(name, func(test *testing.T) {
			resetDecisionLease(test)
			fixture, recorded := readyDecisionDelivery(test)
			placeDecisionHuman(test, fixture.issueID, fixture.humanID, "member", delegate)
			handler, _ := newDecisionRunnerFixture(test)
			if err := handler.deliverDecisionItem(context.Background(), fixture.issue.WorkspaceID, recorded.Request.ID, ""); err != nil {
				test.Fatal(err)
			}
			want := 0
			if delegate {
				want = 1
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, fixture.issueID); count != want {
				test.Fatalf("queued wakes = %d, want %d", count, want)
			}
		})
	}
}

func TestDecisionOwnerWithoutDelegatesUnchanged(test *testing.T) {
	resetDecisionLease(test)
	for _, class := range []string{"reversible", "spend", "deploy", "publish", "irreversible", "product_direction"} {
		test.Run("ping "+class, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			input := fixture.input()
			input.EscalationClass = class
			if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input); err != nil {
				test.Fatal(err)
			}
			fixture.assertCounts(test, 1)
		})
		test.Run("unprompted "+class, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			input := fixture.recordInput()
			input.ParentID, input.EscalationClass = pgtype.UUID{}, class
			if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input); err != nil {
				test.Fatal(err)
			}
		})
	}
	fixture := newAskedRecordFixture(test)
	if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
		test.Fatal(err)
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND status = 'cancelled'`, fixture.issueID) != 0 {
		test.Fatal("lifecycle cancelled an owner's request")
	}
}

func TestUpdateProjectDecisionDelegates(test *testing.T) {
	projectID := dbfx.Project(test, "Delegates API")
	memberID := dbfx.User(test, "Delegate member", "delegate-"+uuid.NewString()+"@test.invalid")
	dbfx.Member(test, testWorkspaceID, memberID, "member")
	outsiderID := dbfx.User(test, "Outsider", "outsider-"+uuid.NewString()+"@test.invalid")
	put := func(userID string, body map[string]any) *httptest.ResponseRecorder {
		request := withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID, body), "id", projectID)
		request.Header.Set("X-User-ID", userID)
		response := httptest.NewRecorder()
		testHandler.UpdateProject(response, request)
		return response
	}
	stored := func() int {
		return dbfx.Count(test, `SELECT count(*) FROM project WHERE id = $1 AND decision_delegates = ARRAY[$2::uuid]`, projectID, memberID)
	}
	if response := put(memberID, map[string]any{"decision_delegates": []string{memberID}}); response.Code != http.StatusForbidden || stored() != 0 {
		test.Fatalf("member set delegates: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := put(testUserID, map[string]any{"decision_delegates": []string{outsiderID}}); response.Code != http.StatusBadRequest || stored() != 0 {
		test.Fatalf("non-member delegate: status=%d body=%s", response.Code, response.Body.String())
	}
	response := put(testUserID, map[string]any{"decision_delegates": []string{memberID, memberID}})
	if response.Code != http.StatusOK || stored() != 1 || !strings.Contains(response.Body.String(), memberID) {
		test.Fatalf("owner set delegates: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := put(testUserID, map[string]any{"title": "renamed"}); response.Code != http.StatusOK || stored() != 1 {
		test.Fatalf("unrelated update changed delegates: status=%d", response.Code)
	}
	if response := put(testUserID, map[string]any{"decision_delegates": nil}); response.Code != http.StatusOK || dbfx.Count(test, `SELECT count(*) FROM project WHERE id = $1 AND decision_delegates = '{}'`, projectID) != 1 {
		test.Fatalf("clear delegates: status=%d body=%s", response.Code, response.Body.String())
	}
	agentID := createHandlerTestAgent(test, "delegates-api-agent", nil)
	request := withURLParam(newRequest(http.MethodPut, "/api/projects/"+projectID, map[string]any{"decision_delegates": []string{memberID}}), "id", projectID)
	response = httptest.NewRecorder()
	testHandler.UpdateProject(response, asAgentActor(request, agentID, createHandlerTestTaskForAgent(test, agentID)))
	if response.Code != http.StatusForbidden || stored() != 0 {
		test.Fatalf("agent set delegates: status=%d body=%s", response.Code, response.Body.String())
	}
}
