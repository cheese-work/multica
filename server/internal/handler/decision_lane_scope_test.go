package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// insertIssueDM inserts a DM that names the issue, so a decision request may go there.
func insertIssueDM(test *testing.T, issueID string, cols testutil.Cols) string {
	test.Helper()
	sessionID := dbfx.Insert(test, "chat_session", cols)
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "decide [the issue](mention://issue/" + issueID + ")"})
	return sessionID
}

// otherIssueFixture is a second issue and running task for the same agent and person.
func otherIssueFixture(test *testing.T, fixture requestCoreFixture) requestCoreFixture {
	test.Helper()
	issueID := dbfx.Issue(test, "other issue", testutil.Cols{"assignee_type": "agent", "assignee_id": fixture.agentID})
	taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	other := fixture.intentFixture
	other.issueID, other.taskID = issueID, taskID
	return newRequestCoreFixtureFor(test, other, false)
}

func TestDecisionRequestRefusesLaneWithoutIssueReference(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	bare := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "Unrelated DM"})
	input := fixture.input()
	input.SessionID = parseUUID(bare)
	_, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
	requireRequestRefusal(test, err)
	if _, matching, _ := strings.Cut(err.Error(), "Matching DMs:"); !strings.Contains(matching, fixture.sessionID) || strings.Contains(matching, bare) {
		test.Fatalf("refusal must list the matching DM only: %v", err)
	}
	fixture.assertCounts(test, 0)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
		test.Fatalf("a DM that names the issue is allowed: %v", err)
	}
}

func TestDecisionNewDMIsForOneIssueOnly(test *testing.T) {
	first := newRequestCoreFixture(test, false)
	result, err := testHandler.createDecisionRequest(first.request(), first.issue, decisionRequestInput{NewDM: true, HumanUserID: parseUUID(first.humanID), Ask: "First issue ask", EscalationClass: "reversible"})
	if err != nil {
		test.Fatal(err)
	}
	second := otherIssueFixture(test, first)
	input := second.input()
	input.SessionID = result.Request.ChatSessionID
	_, err = testHandler.createDecisionRequest(second.request(), second.issue, input)
	requireRequestRefusal(test, err)
	// The same DM stays open to its own issue.
	input = first.input()
	input.HumanUserID = parseUUID(first.humanID)
	input.SessionID = result.Request.ChatSessionID
	input.Ask = "Another ask on the first issue"
	if _, err := testHandler.createDecisionRequest(first.request(), first.issue, input); err != nil {
		test.Fatalf("new DM must serve its own issue: %v", err)
	}
}

func TestDecisionReaskLimit(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	again := fixture.input()
	again.HumanUserID = parseUUID(fixture.humanID)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, again); err != nil {
		test.Fatal(err)
	}
	_, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, again)
	requireRequestRefusal(test, err)
	var asked string
	dbfx.QueryRow(test, `SELECT to_char(created_at AT TIME ZONE 'Asia/Ho_Chi_Minh', 'YYYY-MM-DD HH24:MI') FROM decision_request WHERE issue_id = $1`, fixture.issueID).Scan(&asked)
	if !strings.Contains(err.Error(), asked) {
		test.Fatalf("refusal must carry the earlier ask time %s: %v", asked, err)
	}
	dbfx.Exec(test, `UPDATE decision_request SET created_at = created_at - interval '25 hours' WHERE issue_id = $1`, fixture.issueID)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, again); err != nil {
		test.Fatalf("re-ask after 24 h is allowed: %v", err)
	}
}

func TestDecisionLanesOrderedByIssueReference(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	stale := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "stale reference", "updated_at": time.Now().Add(-time.Hour)})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": stale, "role": "user", "content": "see [the issue](mention://issue/" + fixture.issueID + ")", "created_at": time.Now().Add(-2 * time.Hour)})
	bare := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "unrelated"})
	var lanes []struct {
		ID       string `json:"id"`
		Eligible bool   `json:"eligible"`
	}
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture.intentFixture, "")).Want(http.StatusOK).JSON(&lanes)
	want := []struct {
		id       string
		eligible bool
	}{{fixture.sessionID, true}, {stale, true}, {bare, false}}
	if len(lanes) != len(want) {
		test.Fatalf("lanes = %+v", lanes)
	}
	for index, lane := range lanes {
		if lane.ID != want[index].id || lane.Eligible != want[index].eligible {
			test.Fatalf("lane %d = %+v, want %+v", index, lane, want[index])
		}
	}
}
