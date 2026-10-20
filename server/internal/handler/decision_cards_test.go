package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type recordedCardFixture struct {
	askedRecordFixture
	answerID, commentID, referenceID string
}

func newRecordedCardFixture(test *testing.T) recordedCardFixture {
	test.Helper()
	fixture := newAskedRecordFixture(test)
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	return recordedCardFixture{fixture, uuidToString(result.Answer.ID), uuidToString(result.Comment.ID), uuidToString(result.Message.ID)}
}

func (fixture recordedCardFixture) timelineAs(test *testing.T, userID string) map[string]any {
	test.Helper()
	request := withURLParam(newRequest(http.MethodGet, "/api/issues/"+fixture.issueID+"/timeline", nil), "id", fixture.issueID)
	request.Header.Set("X-User-ID", userID)
	var entries []map[string]any
	testutil.Call(test, testHandler.ListTimeline, request).Want(http.StatusOK).JSON(&entries)
	for _, entry := range entries {
		if entry["id"] == fixture.commentID {
			return entry
		}
	}
	test.Fatalf("quote comment %s missing from timeline", fixture.commentID)
	return nil
}

func (fixture recordedCardFixture) chatMessages(test *testing.T) map[string]map[string]any {
	test.Helper()
	request := withURLParam(httptest.NewRequest(http.MethodGet, "/api/chat/sessions/"+fixture.sessionID+"/messages/page", nil), "sessionId", fixture.sessionID)
	request.Header.Set("X-User-ID", fixture.humanID)
	request = withChatTestWorkspaceCtx(test, request)
	var page struct {
		Messages []map[string]any `json:"messages"`
	}
	testutil.Call(test, testHandler.ListChatMessagesPage, request).Want(http.StatusOK).JSON(&page)
	byID := map[string]map[string]any{}
	for _, message := range page.Messages {
		byID[message["id"].(string)] = message
	}
	return byID
}

func (fixture recordedCardFixture) disclaim(test *testing.T) {
	test.Helper()
	request := withURLParam(newRequest(http.MethodPost, "/api/decision-answers/"+fixture.answerID+"/disclaim", nil), "id", fixture.answerID)
	request.Header.Set("X-User-ID", fixture.humanID)
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	testutil.Call(test, testHandler.DisclaimDecisionAnswer, request).Want(http.StatusOK)
}

func TestDecisionQuoteTimelineCarriesCardFields(test *testing.T) {
	fixture := newRecordedCardFixture(test)
	owner := fixture.timelineAs(test, fixture.humanID)
	for key, wanted := range map[string]string{
		"decision_comment_kind":      "quote",
		"decision_answer_id":         fixture.answerID,
		"decision_human_user_id":     fixture.humanID,
		"decision_recorder_agent_id": fixture.agentID,
		"decision_class":             "reversible",
		"decision_state":             "pending",
		"decision_source_session_id": fixture.sessionID,
		"decision_source_message_id": fixture.messageID,
		"decision_issue_comment_id":  fixture.commentID,
	} {
		if owner[key] != wanted {
			test.Errorf("owner %s = %v, want %q", key, owner[key], wanted)
		}
	}
	if owner["effective_at"] == nil {
		test.Error("owner effective_at is missing")
	}

	other := fixture.timelineAs(test, testUserID)
	if other["decision_state"] != "pending" || other["decision_human_user_id"] != fixture.humanID {
		test.Errorf("non-owner lost the public card fields: %v", other)
	}
	for _, hidden := range []string{"decision_source_session_id", "decision_source_message_id"} {
		if _, leaked := other[hidden]; leaked {
			test.Errorf("non-owner can see private DM pointer %s", hidden)
		}
	}
}

func TestDecisionCardStateFollowsEffectiveAndDisclaim(test *testing.T) {
	fixture := newRecordedCardFixture(test)
	dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, fixture.answerID)
	if state := fixture.timelineAs(test, fixture.humanID)["decision_state"]; state != "effective" {
		test.Fatalf("woken answer state = %v, want effective", state)
	}
	fixture.disclaim(test)
	disclaimed := fixture.timelineAs(test, fixture.humanID)
	if disclaimed["decision_state"] != "disclaimed" || disclaimed["disclaimed_at"] == nil {
		test.Fatalf("disclaimed answer card = %v", disclaimed)
	}
}

func TestChatMessagesCarryDecisionCards(test *testing.T) {
	fixture := newRecordedCardFixture(test)
	messages := fixture.chatMessages(test)

	reference := messages[fixture.referenceID]
	if reference["message_kind"] != "decision_reference" {
		test.Fatalf("decision_reference was downgraded to %v", reference["message_kind"])
	}
	ref, _ := reference["decision_ref"].(map[string]any)
	if ref["answer_id"] != fixture.answerID {
		test.Errorf("reference message is not linked to its answer: %v", reference["decision_ref"])
	}

	quotes, _ := messages[fixture.messageID]["decision_quotes"].([]any)
	if len(quotes) != 1 {
		test.Fatalf("source bubble decision_quotes = %v, want one card", messages[fixture.messageID]["decision_quotes"])
	}
	card := quotes[0].(map[string]any)
	for key, wanted := range map[string]string{
		"answer_id":        fixture.answerID,
		"issue_id":         fixture.issueID,
		"issue_comment_id": fixture.commentID,
		"class":            "reversible",
		"state":            "pending",
	} {
		if card[key] != wanted {
			test.Errorf("card %s = %v, want %q", key, card[key], wanted)
		}
	}
	if card["issue_identifier"] == "" || card["issue_identifier"] == nil {
		test.Error("card has no issue_identifier")
	}

	fixture.disclaim(test)
	after := fixture.chatMessages(test)[fixture.messageID]["decision_quotes"].([]any)[0].(map[string]any)
	if after["state"] != "disclaimed" {
		test.Errorf("card state after disclaim = %v", after["state"])
	}
}

func TestDecisionAnswerUpdatedEventOnRecordAndDisclaim(test *testing.T) {
	var mutex sync.Mutex
	var seen []map[string]any
	testHandler.Bus.Subscribe("decision_answer:updated", func(event events.Event) {
		payload, _ := json.Marshal(event.Payload)
		var decoded map[string]any
		_ = json.Unmarshal(payload, &decoded)
		mutex.Lock()
		seen = append(seen, decoded)
		mutex.Unlock()
	})
	fixture := newAskedRecordFixture(test)
	recordRequest := fixture.recordRequest()
	recordRequest.Body = ioBody(`{"decision_record":true,"content":"note","message_ids":["` + fixture.messageID + `"],"parent_id":"` + fixture.parentID + `"}`)
	recordRequest = withIssueParam(recordRequest, fixture.issueID)
	var quote CommentResponse
	testutil.Call(test, testHandler.CreateComment, recordRequest).Want(http.StatusCreated).JSON(&quote)
	if quote.DecisionAnswerID == nil {
		test.Fatal("record comment carries no answer id")
	}
	card := recordedCardFixture{askedRecordFixture: fixture, answerID: *quote.DecisionAnswerID, commentID: quote.ID}
	card.disclaim(test)

	mutex.Lock()
	defer mutex.Unlock()
	states := []any{}
	for _, payload := range seen {
		if payload["answer_id"] == card.answerID {
			states = append(states, payload["state"])
		}
	}
	if len(states) != 2 || states[0] != "pending" || states[1] != "disclaimed" {
		test.Fatalf("decision_answer:updated states = %v, want [pending disclaimed]", states)
	}
}

func TestDecisionAnswerUpdatedEventWhenDecisionTakesEffect(test *testing.T) {
	resetDecisionLease(test)
	var mutex sync.Mutex
	var states []any
	fixture := newRecordedCardFixture(test)
	runnerHandler, runner := newDecisionRunnerFixture(test)
	runnerHandler.Bus.Subscribe("decision_answer:updated", func(event events.Event) {
		payload, _ := json.Marshal(event.Payload)
		var decoded map[string]any
		_ = json.Unmarshal(payload, &decoded)
		if decoded["answer_id"] == fixture.answerID {
			mutex.Lock()
			states = append(states, decoded["state"])
			mutex.Unlock()
		}
	})
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	decisionPass(test, runner)

	mutex.Lock()
	defer mutex.Unlock()
	if len(states) != 1 || states[0] != "effective" {
		test.Fatalf("decision_answer:updated states on delivery = %v, want [effective]", states)
	}
	if state := fixture.timelineAs(test, fixture.humanID)["decision_state"]; state != "effective" {
		test.Fatalf("timeline state after delivery = %v", state)
	}
}

func TestResolveAndUnresolveKeepDecisionCardFields(test *testing.T) {
	fixture := newRecordedCardFixture(test)
	for _, step := range []string{"resolve", "unresolve"} {
		request := withURLParam(newRequest(http.MethodPost, "/api/comments/"+fixture.commentID+"/"+step, nil), "commentId", fixture.commentID)
		handler := testHandler.ResolveComment
		if step == "unresolve" {
			handler = testHandler.UnresolveComment
		}
		var response map[string]any
		testutil.Call(test, handler, request).Want(http.StatusOK).JSON(&response)
		if response["decision_state"] != "pending" || response["decision_class"] != "reversible" || response["decision_human_user_id"] != fixture.humanID {
			test.Errorf("%s snapshot lost the card fields: %v", step, response)
		}
		for _, private := range []string{"decision_source_session_id", "decision_source_message_id"} {
			if _, leaked := response[private]; leaked {
				test.Errorf("%s snapshot is broadcast and must not carry %s", step, private)
			}
		}
	}
}

func TestForkKeepsDecisionReferenceLink(test *testing.T) {
	_, sourceID, _, _ := setupDirectChatSession(test, context.Background(), "Cards fork")
	answerID := uuid.NewString()
	forkMessage(test, sourceID, testutil.Cols{"role": "assistant", "content": "Decision recorded", "message_kind": "decision_reference", "decision_answer_id": answerID})
	var result ChatForkResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "cards-fork")).Want(http.StatusCreated).JSON(&result)
	cleanupChatFork(test, result.Session.ID)
	var copied string
	dbfx.QueryRow(test, `SELECT coalesce(decision_answer_id::text, '') FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_reference'`, result.Session.ID).Scan(&copied)
	if copied != answerID {
		test.Fatalf("forked reference decision_answer_id = %q, want %q", copied, answerID)
	}
}
