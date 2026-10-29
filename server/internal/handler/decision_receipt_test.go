package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionRequestPersistsDecodedAsk(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated).JSON(&created)
	var raw []byte
	dbfx.QueryRow(test, `SELECT ask_json FROM decision_request WHERE id = $1`, created["id"]).Scan(&raw)
	var stored decisiontx.Ask
	if err := json.Unmarshal(raw, &stored); err != nil || len(stored.Options) != 2 || stored.Options[1].Label != "Hold" || stored.Options[1].Pros[0] != "A pro" || stored.Recommendation != "Hold" {
		test.Fatalf("ask_json = %s (%v)", raw, err)
	}
}

func TestCardAnswerStoresChosenOptionIndexAndReply(test *testing.T) {
	for name, wanted := range map[string]struct {
		input decisionCardAnswerInput
		index int
		reply string
	}{
		"option":      {decisionCardAnswerInput{Option: "Option B"}, 1, ""},
		"option+note": {decisionCardAnswerInput{Option: "Option A", Text: "Do it Friday."}, 0, "Do it Friday."},
		"text":        {decisionCardAnswerInput{Text: "Neither."}, -1, "Neither."},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newCardFixture(test)
			result, err := fixture.answerOnCard(wanted.input)
			if err != nil {
				test.Fatal(err)
			}
			answer := result.Answer
			if wanted.index < 0 && answer.ChosenOptionIndex.Valid || wanted.index >= 0 && (!answer.ChosenOptionIndex.Valid || int(answer.ChosenOptionIndex.Int32) != wanted.index) {
				test.Fatalf("chosen_option_index = %+v, want %d", answer.ChosenOptionIndex, wanted.index)
			}
			if answer.ReplyText.String != wanted.reply {
				test.Fatalf("reply_text = %q, want %q", answer.ReplyText.String, wanted.reply)
			}
		})
	}
}

func TestDMRecordKeepsReplyAndContextLinkWithoutPastedOptions(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	contextID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "assistant", "content": "1) Ship now\n2) Hold", "created_at": "2020-01-01T00:00:00Z"})
	input := fixture.recordInput()
	input.ContextMessageID = parseUUID(contextID)
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(result.Comment.Content, "Options offered") || strings.Contains(result.Comment.Content, "Ship now") {
		test.Fatalf("quote still pastes the earlier reply: %q", result.Comment.Content)
	}
	answer := result.Answer
	if answer.ReplyText.String != "  Yes.\nKeep the exact words.  " || answer.ChosenOptionIndex.Valid || uuidToString(answer.ContextMessageID) != contextID {
		test.Fatalf("record shape: reply=%q index=%+v context=%s", answer.ReplyText.String, answer.ChosenOptionIndex, uuidToString(answer.ContextMessageID))
	}
}

func TestRecordedNoteHasNoRawDisclaimPath(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(result.Message.Content, "/api/") || !strings.Contains(result.Message.Content, "Decision recorded") {
		test.Fatalf("reference note = %q", result.Message.Content)
	}
}

func TestDecisionCardCarriesReceiptFields(test *testing.T) {
	fixture := newCardFixture(test)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
	if err != nil {
		test.Fatal(err)
	}
	answerID := uuidToString(result.Answer.ID)
	card := func() map[string]any {
		messages := recordedCardFixture{fixture, answerID, uuidToString(result.Comment.ID), uuidToString(result.Message.ID)}.chatMessages(test)
		ref, _ := messages[uuidToString(result.Message.ID)]["decision_ref"].(map[string]any)
		return ref
	}
	got := card()
	if got["chosen_option_index"] != float64(1) || got["option_label"] != "Option B" || got["request_message_id"] != uuidToString(fixture.decision.PingMessageID) || got["request_session_id"] != fixture.sessionID {
		test.Fatalf("receipt fields = %v", got)
	}
	if got["delivered"] == true {
		test.Fatalf("pending answer reported delivered: %v", got)
	}
	dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'skipped_ineligible' WHERE id = $1`, answerID)
	if card()["delivered"] == true {
		test.Fatal("skipped_ineligible wake reported delivered")
	}
	dbfx.Exec(test, `UPDATE decision_answer SET wake_outcome = 'run_enqueued' WHERE id = $1`, answerID)
	if card()["delivered"] != true {
		test.Fatal("run_enqueued wake not reported delivered")
	}
}

func TestDecisionRequestCardCarriesAnswerState(test *testing.T) {
	fixture := newCardFixture(test)
	answers := func() map[string]any {
		messages := recordedCardFixture{askedRecordFixture: fixture}.chatMessages(test)
		cards, _ := messages[uuidToString(fixture.decision.PingMessageID)]["decision_request"].(map[string]any)
		answer, _ := cards["answer"].(map[string]any)
		return answer
	}
	if answers() != nil {
		test.Fatal("open card carries an answer")
	}
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
	if err != nil {
		test.Fatal(err)
	}
	if got := answers(); got["state"] != "recorded" || got["option_label"] != "Option B" || got["chosen_option_index"] != float64(1) || got["answered_at"] == nil {
		test.Fatalf("recorded answer = %v", got)
	}
	dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, result.Answer.ID)
	if got := answers(); got["state"] != "delivered" {
		test.Fatalf("delivered answer = %v", got)
	}
	dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE id = $1`, result.Answer.ID)
	dbfx.Exec(test, `UPDATE decision_request SET status = 'open' WHERE id = $1`, fixture.decision.ID)
	if got := answers(); got["state"] != "disclaimed" {
		test.Fatalf("disclaimed answer = %v", got)
	}
}

