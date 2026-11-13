package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// dmProposalFixture is a chat (DM) task of the agent in the human's own DM,
// plus an issue task of the same agent on the anchor issue.
type dmProposalFixture struct {
	singleCallFixture
	sessionID, chatTaskID string
}

func newDMProposalFixture(test *testing.T) dmProposalFixture {
	test.Helper()
	useProposalGuard(test)
	fixture := newProposalFixture(test)
	sessionID := dbfx.ChatSession(test, fixture.agentID, testutil.Cols{"creator_id": fixture.humanID, "explicitly_created_at": testutil.Raw("now()")})
	chatTaskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"chat_session_id": sessionID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	return dmProposalFixture{fixture, sessionID, chatTaskID}
}

// dmBody is a proposal from the chat task: the anchor issue, no person named.
func (fixture dmProposalFixture) dmBody(test *testing.T) map[string]any {
	body := fixture.proposal(test)
	delete(body, "for")
	return body
}

func (fixture dmProposalFixture) dmRequest(body map[string]any) *http.Request {
	request := fixture.proposeRequest(body)
	request.Header.Set("X-Task-ID", fixture.chatTaskID)
	return request
}

func (fixture dmProposalFixture) dmPropose(test *testing.T, body map[string]any, status int) proposalView {
	test.Helper()
	var view proposalView
	testutil.Call(test, testHandler.ProposeDecisionAction, fixture.dmRequest(body)).Want(status).JSON(&view)
	return view
}

func TestProposalFromChatTaskRendersInItsOwnSession(test *testing.T) {
	fixture := newDMProposalFixture(test)
	lanesBefore := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1`, fixture.humanID)
	view := fixture.dmPropose(test, fixture.dmBody(test), http.StatusCreated)
	if view.ChatSessionID != fixture.sessionID || view.HumanUserID != fixture.humanID {
		test.Fatalf("lane = %s recipient = %s, want %s / %s", view.ChatSessionID, view.HumanUserID, fixture.sessionID, fixture.humanID)
	}
	if cards := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID); cards != 1 {
		test.Fatalf("cards in the calling session = %d, want 1", cards)
	}
	if lanes := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1`, fixture.humanID); lanes != lanesBefore {
		test.Fatalf("a new session was created: %d -> %d", lanesBefore, lanes)
	}
	if view.Action == nil || view.Action.ActionType != "agent.copy" {
		test.Fatalf("action = %+v", view.Action)
	}
}

func TestProposalFromChatTaskDoesNotParkTheAnchorIssue(test *testing.T) {
	fixture := newDMProposalFixture(test)
	fixture.dmPropose(test, fixture.dmBody(test), http.StatusCreated)
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issueID).Scan(&status)
	if status != "todo" {
		test.Fatalf("anchor issue status = %q, want it untouched (todo)", status)
	}
}

func TestProposalFromChatTaskReplaysTheOpenRequest(test *testing.T) {
	fixture := newDMProposalFixture(test)
	body := fixture.dmBody(test)
	first := fixture.dmPropose(test, body, http.StatusCreated)
	second := fixture.dmPropose(test, body, http.StatusOK)
	if first.ID != second.ID || fixture.proposalRows(test) != 1 {
		test.Fatalf("ids %s / %s, rows %d", first.ID, second.ID, fixture.proposalRows(test))
	}
}

func TestProposalFromChatTaskNamesOnlyTheSessionCreator(test *testing.T) {
	fixture := newDMProposalFixture(test)
	other := dbfx.User(test, "Other human", "dm-proposal-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "owner"})
	body := fixture.dmBody(test)
	body["for"] = other
	response := testutil.Call(test, testHandler.ProposeDecisionAction, fixture.dmRequest(body))
	if response.Code < 400 {
		test.Fatalf("status = %d, want a refusal for a person who is not the session creator", response.Code)
	}
	if fixture.proposalRows(test) != 0 {
		test.Fatal("a request was written for someone else")
	}
	body["for"] = fixture.humanID // naming the creator is allowed
	fixture.dmPropose(test, body, http.StatusCreated)
}

func TestProposalFromChatTaskRefusesUnavailableSessions(test *testing.T) {
	for _, mutation := range []string{"archived", "intro", "channel", "bound_channel", "task_stopped"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newDMProposalFixture(test)
			switch mutation {
			case "archived":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			case "intro":
				dbfx.Exec(test, `UPDATE chat_session SET is_agent_intro = true WHERE id = $1`, fixture.sessionID)
			case "channel":
				dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, fixture.sessionID)
			case "bound_channel":
				installationID := dbfx.Insert(test, "channel_installation", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "channel_type": "slack", "installer_user_id": testUserID})
				dbfx.Insert(test, "channel_chat_session_binding", testutil.Cols{"chat_session_id": fixture.sessionID, "installation_id": installationID, "channel_type": "slack", "channel_chat_id": "bound-" + fixture.sessionID, "chat_type": "p2p"})
			case "task_stopped":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
			}
			want := http.StatusConflict // a lane that is gone is ineligible, as in the issue lane
			if mutation == "task_stopped" {
				want = http.StatusForbidden
			}
			testutil.Call(test, testHandler.ProposeDecisionAction, fixture.dmRequest(fixture.dmBody(test))).Want(want)
			if fixture.proposalRows(test) != 0 {
				test.Fatal("a refused proposal left a request")
			}
			if cards := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID); cards != 0 {
				test.Fatalf("cards = %d, want 0", cards)
			}
		})
	}
}

func TestProposalFromChatTaskRefusesABadAnchor(test *testing.T) {
	for _, anchor := range []string{"missing", "terminal", "cross_workspace"} {
		test.Run(anchor, func(test *testing.T) {
			fixture := newDMProposalFixture(test)
			body := fixture.dmBody(test)
			want := http.StatusForbidden
			switch anchor {
			case "missing":
				delete(body, "issue_id")
				want = http.StatusNotFound
			case "terminal":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
			case "cross_workspace":
				otherWorkspace := dbfx.Workspace(test, "Other workspace", "dm-proposal-"+uuid.NewString()[:8])
				body["issue_id"] = dbfx.Issue(test, "foreign anchor", testutil.Cols{"workspace_id": otherWorkspace})
				want = http.StatusNotFound
			}
			testutil.Call(test, testHandler.ProposeDecisionAction, fixture.dmRequest(body)).Want(want)
			if fixture.proposalRows(test) != 0 {
				test.Fatal("a refused proposal left a request")
			}
		})
	}
}

func TestProposalOfTheSameActionFromTheIssueTaskAndTheDMAreTwoRequestsInOneDM(test *testing.T) {
	fixture := newDMProposalFixture(test)
	body := fixture.proposal(test)
	fromIssue := fixture.propose(test, body, http.StatusCreated)
	fromDM := fixture.dmPropose(test, fixture.dmBody(test), http.StatusCreated)
	if fromIssue.ID == fromDM.ID || fromIssue.ChatSessionID != fixture.sessionID || fromDM.ChatSessionID != fixture.sessionID {
		test.Fatalf("issue request %s in %s, DM request %s in %s, want two requests in %s", fromIssue.ID, fromIssue.ChatSessionID, fromDM.ID, fromDM.ChatSessionID, fixture.sessionID)
	}
	if fixture.proposalRows(test) != 2 {
		test.Fatalf("rows = %d, want 2", fixture.proposalRows(test))
	}
	if !strings.Contains(fromDM.Ask, "Proposed action") {
		test.Fatalf("ask = %q", fromDM.Ask)
	}
}

// An issue task asks the person in the DM they already have with the agent,
// the most recently active one, not in a lane they never opened (CHE-5253).
func TestProposalFromIssueTaskLandsInTheLatestDMWithTheAgent(test *testing.T) {
	fixture := newDMProposalFixture(test)
	older := dbfx.ChatSession(test, fixture.agentID, testutil.Cols{"creator_id": fixture.humanID, "explicitly_created_at": testutil.Raw("now()")})
	dbfx.Exec(test, `UPDATE chat_session SET updated_at = now() - interval '1 hour' WHERE id = $1`, older)
	dbfx.Exec(test, `UPDATE chat_session SET updated_at = now() WHERE id = $1`, fixture.sessionID)
	lanesBefore := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1`, fixture.humanID)
	view := fixture.propose(test, fixture.proposal(test), http.StatusCreated)
	if view.ChatSessionID != fixture.sessionID {
		test.Fatalf("card landed in %s, want the latest DM %s", view.ChatSessionID, fixture.sessionID)
	}
	if lanes := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE creator_id = $1`, fixture.humanID); lanes != lanesBefore {
		test.Fatalf("a new session was created: %d -> %d", lanesBefore, lanes)
	}
}

func TestProposalFromIssueTaskStillNeedsANamedPerson(test *testing.T) {
	useProposalGuard(test)
	fixture := newProposalFixture(test)
	body := fixture.proposal(test)
	delete(body, "for")
	testutil.Call(test, testHandler.ProposeDecisionAction, fixture.proposeRequest(body)).Want(http.StatusBadRequest)
}
