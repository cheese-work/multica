package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// askUnder runs a one-call decision request, to the test user, from a fresh
// agent on a new issue placed under parentID and/or projectID.
func askUnder(test *testing.T, parentID, projectID string) (singleCallFixture, map[string]any) {
	test.Helper()
	fixture := newSingleCallFixture(test)
	if parentID != "" {
		dbfx.Exec(test, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, fixture.issueID, parentID)
	}
	if projectID != "" {
		dbfx.Exec(test, `UPDATE issue SET project_id = $2 WHERE id = $1`, fixture.issueID, projectID)
	}
	body := fixture.body()
	body["for"] = testUserID
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated).JSON(&created)
	sessionID, _ := created["chat_session_id"].(string)
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	return fixture, created
}

func sessionOf(test *testing.T, created map[string]any) (sessionID, agentID, title string) {
	test.Helper()
	sessionID, _ = created["chat_session_id"].(string)
	dbfx.QueryRow(test, `SELECT agent_id::text, title FROM chat_session WHERE id = $1`, sessionID).Scan(&agentID, &title)
	return sessionID, agentID, title
}

func TestDecisionTopicSharesOneDMPerRoot(test *testing.T) {
	steward := createHandlerTestAgent(test, "topic-steward-"+uuid.NewString()[:8], []byte("[]"))
	root := dbfx.Issue(test, "Root epic", testutil.Cols{"assignee_type": "agent", "assignee_id": steward})
	first, firstCreated := askUnder(test, root, "")
	second, secondCreated := askUnder(test, root, "")
	firstSession, firstAgent, title := sessionOf(test, firstCreated)
	secondSession, _, _ := sessionOf(test, secondCreated)
	if first.agentID == second.agentID {
		test.Fatal("fixture must use two asking agents")
	}
	if firstSession != secondSession {
		test.Fatalf("two agents under one root opened two DMs: %s / %s", firstSession, secondSession)
	}
	if firstAgent != steward || title != "Decisions: Root epic" {
		test.Fatalf("DM agent=%s title=%q, want steward %s titled for the root", firstAgent, title, steward)
	}
}

func TestDecisionTopicAndSteward(test *testing.T) {
	leader := createHandlerTestAgent(test, "squad-leader-"+uuid.NewString()[:8], []byte("[]"))
	squad := dbfx.Squad(test, "topic squad", leader)
	project := dbfx.Project(test, "Topic project")
	for _, rule := range []struct {
		name, title string
		rootFields  testutil.Cols
		useProject  bool
		wantLeader  bool
	}{
		{name: "squad root steward is the leader", title: "Decisions: Squad root", rootFields: testutil.Cols{"assignee_type": "squad", "assignee_id": squad}, wantLeader: true},
		{name: "unassigned root falls back to the asker", title: "Decisions: Bare root"},
		{name: "member-assigned root falls back to the asker", title: "Decisions: Member root", rootFields: testutil.Cols{"assignee_type": "member", "assignee_id": testUserID}},
		{name: "no parent uses the project", title: "Decisions: Topic project", useProject: true},
		{name: "no parent and no project is the issue itself", title: "Decisions: intent issue"},
	} {
		test.Run(rule.name, func(test *testing.T) {
			var fixture singleCallFixture
			var created map[string]any
			switch {
			case rule.useProject:
				fixture, created = askUnder(test, "", project)
			case rule.rootFields == nil && rule.title == "Decisions: intent issue":
				fixture, created = askUnder(test, "", "")
			default:
				root := dbfx.Issue(test, rule.title[len("Decisions: "):], rule.rootFields)
				fixture, created = askUnder(test, root, "")
			}
			_, agentID, title := sessionOf(test, created)
			want := fixture.agentID
			if rule.wantLeader {
				want = leader
			}
			if agentID != want || title != rule.title {
				test.Fatalf("DM agent=%s title=%q, want agent %s title %q", agentID, title, want, rule.title)
			}
		})
	}
}

// foreignAsk is a request by agent A in the DM that steward S owns.
func foreignAsk(test *testing.T) (fixture singleCallFixture, requestID, sessionID, stewardID string) {
	test.Helper()
	stewardID = createHandlerTestAgent(test, "topic-steward-"+uuid.NewString()[:8], []byte("[]"))
	root := dbfx.Issue(test, "Reply root", testutil.Cols{"assignee_type": "agent", "assignee_id": stewardID})
	fixture, created := askUnder(test, root, "")
	requestID, _ = created["id"].(string)
	sessionID, _, _ = sessionOf(test, created)
	return fixture, requestID, sessionID, stewardID
}

func replyToDecision(test *testing.T, sessionID, requestID, source string) *testutil.Response {
	test.Helper()
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": "> Approve the reversible change?\n\nProceed", "decision_request_id": requestID})
	request.Header.Set("X-Actor-Source", source)
	request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
	return testutil.Call(test, testHandler.SendChatMessage, request)
}

func TestDecisionTopicReplyStartsNoStewardTask(test *testing.T) {
	fixture, requestID, sessionID, _ := foreignAsk(test)
	var sent SendChatMessageResponse
	replyToDecision(test, sessionID, requestID, "interactive_jwt").Want(http.StatusCreated).JSON(&sent)
	if sent.TaskID != "" || dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID) != 0 {
		test.Fatalf("a bound reply started a steward task: %q", sent.TaskID)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer a JOIN decision_request r ON r.id = a.decision_request_id WHERE r.id = $1 AND r.status = 'answered' AND a.answered_by_task_id IS NULL AND a.wake_at IS NOT NULL AND a.chat_message_ids = ARRAY[$2::uuid]`, requestID, sent.MessageID) != 1 {
		test.Fatal("answer was not recorded task-less and ready to wake the asker")
	}
	if dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'quote' AND author_id = $2`, fixture.issueID, fixture.agentID) != 1 {
		test.Fatal("the quote must be attributed to the asking agent")
	}
}

func TestDecisionTopicReplyRefusals(test *testing.T) {
	test.Run("attestation", func(test *testing.T) {
		_, requestID, sessionID, _ := foreignAsk(test)
		before := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID)
		replyToDecision(test, sessionID, requestID, "pat").Want(http.StatusForbidden)
		if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID) != before || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID) != 0 {
			test.Fatal("refused reply wrote a message or answer")
		}
	})
	test.Run("session outside the topic", func(test *testing.T) {
		_, requestID, sessionID, _ := foreignAsk(test)
		dbfx.Exec(test, `UPDATE chat_session SET decision_topic_id = NULL WHERE id = $1`, sessionID)
		replyToDecision(test, sessionID, requestID, "interactive_jwt").Want(http.StatusForbidden)
		if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID) != 0 {
			test.Fatal("refused reply wrote an answer")
		}
	})
	test.Run("explicit lane of another topic", func(test *testing.T) {
		_, created := askUnder(test, dbfx.Issue(test, "Other root"), "")
		laneID, _, _ := sessionOf(test, created)
		stranger := newSingleCallFixture(test)
		parent := dbfx.Issue(test, "Unrelated root")
		dbfx.Exec(test, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, stranger.issueID, parent)
		body := stranger.body()
		body["for"], body["session_id"] = testUserID, laneID
		testutil.Call(test, testHandler.RequestDecision, stranger.call(body)).Want(http.StatusForbidden)
	})
}

func TestDecisionTopicStewardFollowsRootAssignee(test *testing.T) {
	_, _, sessionID, steward := foreignAsk(test)
	successor := createHandlerTestAgent(test, "topic-successor-"+uuid.NewString()[:8], []byte("[]"))
	var rootID string
	dbfx.QueryRow(test, `SELECT decision_topic_id::text FROM chat_session WHERE id = $1`, sessionID).Scan(&rootID)
	testutil.Call(test, testHandler.UpdateIssue, testutil.WithURLParams(
		newRequest(http.MethodPatch, "/api/issues/"+rootID, map[string]any{"assignee_type": "agent", "assignee_id": successor}), "id", rootID,
	)).Want(http.StatusOK)
	var agentID string
	dbfx.QueryRow(test, `SELECT agent_id::text FROM chat_session WHERE id = $1`, sessionID).Scan(&agentID)
	if agentID != successor || agentID == steward {
		test.Fatalf("DM agent = %s, want the new root assignee %s", agentID, successor)
	}
	if dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE id = $1 AND status = 'active'`, sessionID) != 1 {
		test.Fatal("the DM itself must stay")
	}
}

func TestDecisionTopicReplySurvivesReparent(test *testing.T) {
	fixture, requestID, sessionID, _ := foreignAsk(test)
	dbfx.Exec(test, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, fixture.issueID, dbfx.Issue(test, "Elsewhere"))
	replyToDecision(test, sessionID, requestID, "interactive_jwt").Want(http.StatusCreated)
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, requestID) != 1 {
		test.Fatal("an ask whose issue moved out of the topic was stranded unanswered")
	}
}

func TestDecisionTopicConcurrentAsksShareOneDM(test *testing.T) {
	steward := createHandlerTestAgent(test, "topic-steward-"+uuid.NewString()[:8], []byte("[]"))
	root := dbfx.Issue(test, "Racing root", testutil.Cols{"assignee_type": "agent", "assignee_id": steward})
	var fixtures [2]singleCallFixture
	for i := range fixtures {
		fixtures[i] = newSingleCallFixture(test)
		dbfx.Exec(test, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, fixtures[i].issueID, root)
	}
	var wait sync.WaitGroup
	var codes [2]int
	var sessions [2]string
	for i := range fixtures {
		wait.Add(1)
		go func() {
			defer wait.Done()
			body := fixtures[i].body()
			body["for"] = testUserID
			recorder := httptest.NewRecorder()
			testHandler.RequestDecision(recorder, fixtures[i].call(body))
			codes[i] = recorder.Code
			var created map[string]any
			_ = json.Unmarshal(recorder.Body.Bytes(), &created)
			sessions[i], _ = created["chat_session_id"].(string)
		}()
	}
	wait.Wait()
	test.Cleanup(func() {
		for _, id := range sessions {
			dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, id)
			dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, id)
		}
	})
	if codes[0] != http.StatusCreated || codes[1] != http.StatusCreated {
		test.Fatalf("concurrent asks returned %v", codes)
	}
	if sessions[0] != sessions[1] || dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE decision_topic_id = $1`, root) != 1 {
		test.Fatalf("concurrent asks opened %s and %s, want one DM", sessions[0], sessions[1])
	}
}

func TestDecisionTopicStewardStaysWhenPersonCannotInvokeSuccessor(test *testing.T) {
	_, _, sessionID, steward := foreignAsk(test)
	successor := createHandlerTestAgent(test, "topic-closed-"+uuid.NewString()[:8], []byte("[]"))
	dbfx.Exec(test, `DELETE FROM agent_invocation_target WHERE agent_id = $1`, successor)
	dbfx.Exec(test, `UPDATE agent SET owner_id = NULL WHERE id = $1`, successor)
	var rootID string
	dbfx.QueryRow(test, `SELECT decision_topic_id::text FROM chat_session WHERE id = $1`, sessionID).Scan(&rootID)
	dbfx.Exec(test, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, rootID, successor)
	root, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(rootID))
	if err != nil {
		test.Fatal(err)
	}
	testHandler.followDecisionSteward(context.Background(), root)
	var agentID string
	dbfx.QueryRow(test, `SELECT agent_id::text FROM chat_session WHERE id = $1`, sessionID).Scan(&agentID)
	if agentID != steward {
		test.Fatalf("DM moved to %s, an agent its person cannot invoke; want it to stay with %s", agentID, steward)
	}
}
