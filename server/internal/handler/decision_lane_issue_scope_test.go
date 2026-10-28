package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// otherIssueFixture is a second running task of the same agent on a different
// issue, asking the same person.
func otherIssueFixture(test *testing.T, base singleCallFixture) singleCallFixture {
	test.Helper()
	issueID := dbfx.Issue(test, "other issue", testutil.Cols{"assignee_type": "agent", "assignee_id": base.agentID})
	taskID := dbfx.Task(test, base.agentID, testutil.Cols{"issue_id": issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(test, `DELETE FROM decision_request WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(test, `DELETE FROM decision_intent WHERE issue_id = $1`, issueID)
	sourceID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "author_type": "agent", "author_id": base.agentID, "source_task_id": taskID, "content": "Other source context."})
	details := structuredDecisionAsk(issueID, sourceID, "Approve the other change?")
	return singleCallFixture{intentFixture: intentFixture{base.agentID, issueID, taskID, base.humanID}, ask: details.Content(handlerTestWorkspaceSlug), footer: "Next owner: none", sourceID: sourceID, details: details}
}

// laneNamingIssue is a DM lane whose history mentions the issue, so decision
// requests for it are accepted there.
func laneNamingIssue(test *testing.T, issueID, agentID, creatorID, title string) string {
	test.Helper()
	sessionID := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": creatorID, "title": title})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "see [issue](mention://issue/" + issueID + ")"})
	return sessionID
}

func (fixture singleCallFixture) newDMLane(test *testing.T, title string) string {
	return dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": title})
}

func (fixture singleCallFixture) discussIn(test *testing.T, sessionID string) {
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "see [X-1](mention://issue/" + fixture.issueID + ") please"})
}

func TestDecisionRequestRefusesLaneThatNeverNamedTheIssue(test *testing.T) {
	fixture := newSingleCallFixture(test)
	unrelated := fixture.newDMLane(test, "unrelated")
	matching := fixture.newDMLane(test, "discussed here")
	fixture.discussIn(test, matching)
	body := fixture.body()
	body["session_id"] = unrelated
	var refusal map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusForbidden).JSON(&refusal)
	if message, _ := refusal["error"].(string); !strings.Contains(message, matching) {
		test.Fatalf("refusal must list the matching DM: %v", refusal)
	}
	if comments, messages, pings := fixture.counts(test); comments != 0 || messages != 1 || pings != 0 {
		test.Fatalf("refused request wrote comments=%d messages=%d pings=%d", comments, messages, pings)
	}
	body["session_id"] = matching
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
}

func TestDecisionRequestNewDMIsOnlyForItsIssue(test *testing.T) {
	first := newSingleCallFixture(test)
	body := first.body()
	body["new_dm"] = true
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, first.call(body)).Want(http.StatusCreated).JSON(&created)
	second := otherIssueFixture(test, first)
	reuse := second.body()
	reuse["session_id"] = created["chat_session_id"]
	testutil.Call(test, testHandler.RequestDecision, second.call(reuse)).Want(http.StatusForbidden)
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, second.issueID) != 0 {
		test.Fatal("refused request left a decision request on the second issue")
	}
	again := first.body()
	again["session_id"] = created["chat_session_id"]
	again["ask"] = structuredDecisionAsk(first.issueID, first.sourceID, "A second question on the first issue?")
	testutil.Call(test, testHandler.RequestDecision, first.call(again)).Want(http.StatusCreated)
}

func TestDecisionRequestReaskLimit(test *testing.T) {
	fixture := newSingleCallFixture(test)
	lane := fixture.newDMLane(test, "discussed here")
	fixture.discussIn(test, lane)
	body := fixture.body()
	body["session_id"] = lane
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
	// A later run of the same agent asks the same open decision again.
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	fixture.taskID = dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	var refusal map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusConflict).JSON(&refusal)
	if message, _ := refusal["error"].(string); !strings.Contains(message, "ICT") {
		test.Fatalf("refusal must name the time of the earlier ask: %v", refusal)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID) != 1 {
		test.Fatal("refused re-ask left a second request")
	}
	dbfx.Exec(test, `UPDATE decision_request SET created_at = now() - interval '25 hours' WHERE issue_id = $1`, fixture.issueID)
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
}

func TestDecisionLanesAreOrderedByIssueRelevance(test *testing.T) {
	fixture := newIntentFixture(test)
	openLaneIntent(test, fixture, fixture.mention())
	lane := func(title string, updated time.Time) string {
		return dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": title, "updated_at": updated})
	}
	now := time.Now()
	unrelated := lane("unrelated", now)
	older := lane("older discussion", now.Add(-3*time.Hour))
	newer := lane("newer discussion", now.Add(-2*time.Hour))
	mention := "[X-1](mention://issue/" + fixture.issueID + ")"
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": older, "role": "user", "content": mention, "created_at": now.Add(-5 * time.Hour)})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": newer, "role": "user", "content": mention, "created_at": now.Add(-4 * time.Hour)})
	var lanes []struct {
		ID       string `json:"id"`
		Eligible bool   `json:"eligible"`
	}
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "")).Want(http.StatusOK).JSON(&lanes)
	if len(lanes) != 3 || lanes[0].ID != newer || lanes[1].ID != older || lanes[2].ID != unrelated {
		test.Fatalf("lanes not ordered by latest message naming the issue: %+v", lanes)
	}
	if !lanes[0].Eligible || !lanes[1].Eligible || lanes[2].Eligible {
		test.Fatalf("eligibility must follow the reference: %+v", lanes)
	}
}

func TestDecisionRequestKeepsOlderDiscussedDMEligible(test *testing.T) {
	fixture := newSingleCallFixture(test)
	matching := fixture.newDMLane(test, "original discussion")
	fixture.discussIn(test, matching)
	dbfx.Exec(test, `UPDATE chat_session SET updated_at = now() - interval '2 days' WHERE id = $1`, matching)
	for index := 0; index < 10; index++ {
		fixture.newDMLane(test, "new unrelated conversation")
	}
	body := fixture.body()
	body["session_id"] = matching
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
}

func TestDecisionLanesRankOlderDiscussedDMBeforeTheLimit(test *testing.T) {
	fixture := newIntentFixture(test)
	openLaneIntent(test, fixture, fixture.mention())
	matching := laneNamingIssue(test, fixture.issueID, fixture.agentID, fixture.humanID, "original discussion")
	dbfx.Exec(test, `UPDATE chat_session SET updated_at = now() - interval '2 days' WHERE id = $1`, matching)
	for index := 0; index < 10; index++ {
		dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "new unrelated conversation"})
	}
	var lanes []struct {
		ID       string `json:"id"`
		Eligible bool   `json:"eligible"`
	}
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "")).Want(http.StatusOK).JSON(&lanes)
	if len(lanes) == 0 || lanes[0].ID != matching || !lanes[0].Eligible {
		test.Fatalf("the original issue DM must rank first, got %+v", lanes)
	}
}

func TestDecisionReaskWithFreshSourceCommentIsRefused(test *testing.T) {
	fixture := newSingleCallFixture(test)
	lane := laneNamingIssue(test, fixture.issueID, fixture.agentID, fixture.humanID, "original discussion")
	body := fixture.body()
	body["session_id"] = lane
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	fixture.taskID = dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	sourceID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "agent", "author_id": fixture.agentID, "source_task_id": fixture.taskID, "content": "Scoped source context."})
	fixture.details.SourceCommentID = sourceID
	body = fixture.body()
	body["session_id"] = lane
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusConflict)
}
