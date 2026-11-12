package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// dmDecisionFixture is a running chat task of the agent in the human's own DM,
// plus the issue task of the single-call fixture that anchors the issue.
type dmDecisionFixture struct {
	singleCallFixture
	sessionID, chatTaskID string
}

func newDMDecisionFixture(test *testing.T) dmDecisionFixture {
	test.Helper()
	fixture := newSingleCallFixture(test)
	sessionID := dbfx.ChatSession(test, fixture.agentID, testutil.Cols{"creator_id": fixture.humanID, "explicitly_created_at": testutil.Raw("now()")})
	chatTaskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"chat_session_id": sessionID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	return dmDecisionFixture{fixture, sessionID, chatTaskID}
}

func (fixture dmDecisionFixture) request(body map[string]any) *http.Request {
	request := fixture.call(body)
	request.Header.Set("X-Task-ID", fixture.chatTaskID)
	return request
}

func (fixture dmDecisionFixture) dmBody() map[string]any {
	body := fixture.body()
	delete(body, "for")
	delete(body, "footer")
	return body
}

type dmDecisionView struct {
	ID              string `json:"id"`
	IssueID         string `json:"issue_id"`
	HumanUserID     string `json:"human_user_id"`
	ChatSessionID   string `json:"chat_session_id"`
	EscalationClass string `json:"escalation_class"`
	Kind            string `json:"kind"`
	ExpiresAt       string `json:"expires_at"`
	Status          string `json:"status"`
}

func TestDecisionRequestFromChatTaskBindsToItsOwnDM(test *testing.T) {
	fixture := newDMDecisionFixture(test)
	lanesBefore := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1`, fixture.humanID)
	var view dmDecisionView
	testutil.Call(test, testHandler.RequestDecision, fixture.request(fixture.dmBody())).Want(http.StatusCreated).JSON(&view)
	if view.ID == "" || view.IssueID != fixture.issueID || view.HumanUserID != fixture.humanID || view.ChatSessionID != fixture.sessionID || view.EscalationClass != "reversible" || view.Status != "open" {
		test.Fatalf("stored request = %+v, want id, issue %s, member %s, session %s, scope reversible", view, fixture.issueID, fixture.humanID, fixture.sessionID)
	}
	var stored dmDecisionView
	dbfx.QueryRow(test, `SELECT id::text, issue_id::text, human_user_id::text, chat_session_id::text, escalation_class, status FROM decision_request WHERE id = $1`, view.ID).
		Scan(&stored.ID, &stored.IssueID, &stored.HumanUserID, &stored.ChatSessionID, &stored.EscalationClass, &stored.Status)
	if stored.IssueID != view.IssueID || stored.HumanUserID != view.HumanUserID || stored.ChatSessionID != view.ChatSessionID || stored.EscalationClass != view.EscalationClass {
		test.Fatalf("read back %+v, want %+v", stored, view)
	}
	if cards := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID); cards != 1 {
		test.Fatalf("cards in the calling DM = %d, want 1", cards)
	}
	if lanes := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1`, fixture.humanID); lanes != lanesBefore {
		test.Fatalf("a new session was created: %d -> %d", lanesBefore, lanes)
	}
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issueID).Scan(&status)
	if status != "todo" {
		test.Fatalf("anchor issue status = %q, want it untouched (todo)", status)
	}
}

func TestDecisionRequestFromChatTaskCarriesExpiry(test *testing.T) {
	enableActionRequests(test, true)
	fixture := newDMDecisionFixture(test)
	body := fixture.dmBody()
	body["kind"], body["expires_in"] = "action", "24h"
	body["ask"] = map[string]any{
		"context_eli10": "A runner needs a one-time login. Only you can do it.", "agent_did": "Prepared the runner.",
		"why_human": "The login needs your account.", "steps": []string{"Open the runner page and sign in."},
		"done_when": "The runner shows Connected.", "default_if_no_reply": "The runner stays off.",
		"source_issue_id": fixture.issueID, "source_comment_id": fixture.sourceID,
	}
	before := time.Now()
	var view dmDecisionView
	testutil.Call(test, testHandler.RequestDecision, fixture.request(body)).Want(http.StatusCreated).JSON(&view)
	expires, err := time.Parse(time.RFC3339Nano, view.ExpiresAt)
	if err != nil || expires.Before(before.Add(23*time.Hour)) || expires.After(before.Add(25*time.Hour)) {
		test.Fatalf("expires_at = %q (%v), want about 24h ahead", view.ExpiresAt, err)
	}
	if view.Kind != "action" || view.ChatSessionID != fixture.sessionID {
		test.Fatalf("stored request = %+v", view)
	}
}

func TestDecisionRequestFromChatTaskNamesOnlyTheSessionCreator(test *testing.T) {
	fixture := newDMDecisionFixture(test)
	other := dbfx.User(test, "Other human", "dm-decision-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "owner"})
	body := fixture.dmBody()
	body["for"] = other
	if status := testutil.Call(test, testHandler.RequestDecision, fixture.request(body)).Code; status != http.StatusForbidden {
		test.Fatalf("status = %d, want 403 for a person who is not the DM's member", status)
	}
	if rows := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); rows != 0 {
		test.Fatalf("a request was written for someone else: %d rows", rows)
	}
	body["for"] = fixture.humanID // naming the DM's own member is allowed
	testutil.Call(test, testHandler.RequestDecision, fixture.request(body)).Want(http.StatusCreated)
}

func TestDecisionRequestFromChatTaskRefusesUnavailableDM(test *testing.T) {
	for _, mutation := range []string{"archived", "intro", "channel", "task_stopped", "other_agent_session"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newDMDecisionFixture(test)
			switch mutation {
			case "archived":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			case "intro":
				dbfx.Exec(test, `UPDATE chat_session SET is_agent_intro = true WHERE id = $1`, fixture.sessionID)
			case "channel":
				dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, fixture.sessionID)
			case "task_stopped":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
			case "other_agent_session":
				dbfx.Exec(test, `UPDATE chat_session SET agent_id = $1 WHERE id = $2`, createHandlerTestAgent(test, "other dm agent", []byte("[]")), fixture.sessionID)
			}
			if status := testutil.Call(test, testHandler.RequestDecision, fixture.request(fixture.dmBody())).Code; status < 400 || status >= 500 {
				test.Fatalf("status = %d, want a 4xx refusal", status)
			}
			if rows := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); rows != 0 {
				test.Fatalf("refusal left %d request rows", rows)
			}
		})
	}
}
