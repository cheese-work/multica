package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The issue page folds a card that is used, so it needs the used cards, not only the open ones (CHE-4431).
func TestListIssueDecisionCardsReturnsUsedCardsOfTheViewerOnly(test *testing.T) {
	fixture := newCardFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET ping_message_id = $2 WHERE id = $1`, fixture.decision.ID, fixture.messageID)
	listAs := func(userID string) []DecisionRequestCard {
		request := newRequest(http.MethodGet, "/api/decisions/issue-cards?issue="+fixture.issueID, nil)
		request.Header.Set("X-User-ID", userID)
		var cards []DecisionRequestCard
		testutil.Call(test, testHandler.ListIssueDecisionCards, request).Want(http.StatusOK).JSON(&cards)
		return cards
	}
	if cards := listAs(fixture.humanID); len(cards) != 0 {
		test.Fatalf("an open request is listed as used: %+v", cards)
	}
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatal(err)
	}
	cards := listAs(fixture.humanID)
	if len(cards) != 1 || cards[0].ID != uuidToString(fixture.decision.ID) || cards[0].State != "answered" || cards[0].Answer == nil || cards[0].Answer.OptionLabel != "Option A" {
		test.Fatalf("answered card missing or wrong: %+v", cards)
	}
	if cards := listAs(testUserID); len(cards) != 0 {
		test.Fatalf("another member's card was listed: %+v", cards)
	}
}
