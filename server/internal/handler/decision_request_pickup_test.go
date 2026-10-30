package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func pickupCard(test *testing.T, fixture askedRecordFixture, request db.DecisionRequest) DecisionRequestCard {
	test.Helper()
	messages := []db.ChatMessage{{ID: parseUUID(fixture.messageID), ChatSessionID: parseUUID(fixture.sessionID)}}
	dbfx.Exec(test, `UPDATE decision_request SET ping_message_id = $2 WHERE id = $1`, request.ID, fixture.messageID)
	cards := testHandler.decisionRequestsForMessages(context.Background(), testWorkspaceID, messages)[fixture.messageID]
	if len(cards) != 1 {
		test.Fatalf("cards = %+v", cards)
	}
	return cards[0]
}

// Recorded → Picked up: only a run on the answered issue, after the record, counts.
func TestDecisionRequestCardShowsTheRunThatPickedUpTheAnswer(test *testing.T) {
	fixture := newCardFixture(test)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"})
	if err != nil {
		test.Fatal(err)
	}
	card := pickupCard(test, fixture, result.Request)
	if card.Answer == nil || card.Answer.State == "picked_up" || card.Answer.Run != nil {
		test.Fatalf("no run yet, answer = %+v", card.Answer)
	}

	otherIssue := dbfx.Issue(test, "unrelated issue")
	dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": otherIssue, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	if card = pickupCard(test, fixture, result.Request); card.Answer.Run != nil || card.Answer.State == "picked_up" {
		test.Fatalf("a run on another issue counted: %+v", card.Answer)
	}

	runID := dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	card = pickupCard(test, fixture, result.Request)
	if card.Answer.State != "picked_up" || card.Answer.Run == nil || card.Answer.Run.ID != runID || card.Answer.Run.AgentName == "" {
		test.Fatalf("run on the issue not shown: %+v", card.Answer)
	}

	dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE id = $1`, result.Answer.ID)
	if card = pickupCard(test, fixture, result.Request); card.Answer.State != "disclaimed" {
		test.Fatalf("a disclaimed answer shows %q", card.Answer.State)
	}
}
