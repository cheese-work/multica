package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// askAs posts one more structured request from the fixture's agent to its human in the same DM.
func (fixture requestCoreFixture) askAs(test *testing.T, decision string) db.DecisionRequest {
	test.Helper()
	body := map[string]any{
		"issue_id": fixture.issueID, "session_id": fixture.sessionID, "for": fixture.humanID, "escalation_class": "reversible",
		"ask": structuredDecisionAsk(fixture.issueID, fixture.tagID, decision),
	}
	request := newRequest(http.MethodPost, "/api/decisions", body)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	var created db.DecisionRequest
	testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusCreated).JSON(&created)
	return created
}

func TestDecisionPingCoalescesSecondAskFromSameAgent(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	first := fixture.askAs(test, "Approve the first change?")
	second := fixture.askAs(test, "Approve the second change?")
	third := fixture.askAs(test, "Approve the third change?")
	if first.ID == second.ID || second.ID == third.ID {
		test.Fatal("each ask keeps its own request")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); n != 1 {
		test.Fatalf("inbox items = %d, want 1: later asks must not ping again", n)
	}
	var body string
	dbfx.QueryRow(test, `SELECT body FROM inbox_item WHERE issue_id = $1`, fixture.issueID).Scan(&body)
	if want := "3 open asks from " + fixture.agentName; body != want {
		test.Fatalf("body = %q, want %q", body, want)
	}
	// Retry after coalesce: the receipt is durable, so the worker selects nothing and a direct retry creates no ping.
	pending, err := testHandler.Queries.ListDecisionPingRetries(context.Background(), 100)
	if err != nil {
		test.Fatal(err)
	}
	for _, request := range pending {
		if request.ID == second.ID || request.ID == third.ID {
			test.Fatal("coalesced request is still a ping retry candidate")
		}
	}
	item, err := decisiontx.NotifyPing(context.Background(), testPool, testHandler.Queries, second.WorkspaceID, second.ID)
	if err != nil || item != nil {
		test.Fatalf("retry after coalesce: item=%+v err=%v", item, err)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); n != 1 {
		test.Fatalf("inbox items after retry = %d, want 1", n)
	}
}

func repingCandidates(test *testing.T) map[string]bool {
	test.Helper()
	rows, err := testHandler.Queries.ListDecisionPingRepings(context.Background(), 100)
	if err != nil {
		test.Fatal(err)
	}
	ids := map[string]bool{}
	for _, row := range rows {
		ids[uuidToString(row.ID)] = true
	}
	return ids
}

func TestDecisionPingRepingsOnceWhenPingedAskIsClosed(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	first := fixture.askAs(test, "Approve the first change?")
	second := fixture.askAs(test, "Approve the second change?")
	third := fixture.askAs(test, "Approve the third change?")
	if repingCandidates(test)[uuidToString(second.ID)] {
		test.Fatal("no re-ping while the pinged ask is open")
	}
	dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'issue_closed' WHERE id = $1`, first.ID)
	dbfx.Exec(test, `UPDATE inbox_item SET read = true WHERE issue_id = $1`, fixture.issueID)
	candidates := repingCandidates(test)
	if !candidates[uuidToString(second.ID)] || candidates[uuidToString(third.ID)] {
		test.Fatalf("want exactly the oldest coalesced ask, got %v", candidates)
	}
	for attempt := 0; attempt < 2; attempt++ {
		item, err := decisiontx.RepingCoalesced(context.Background(), testPool, testHandler.Queries, second.WorkspaceID, second.ID)
		if err != nil || (attempt == 0) != (item != nil) {
			test.Fatalf("re-ping %d: item=%+v err=%v", attempt, item, err)
		}
	}
	var unread int
	var body string
	dbfx.QueryRow(test, `SELECT count(*), max(body) FROM inbox_item WHERE issue_id = $1 AND read = false`, fixture.issueID).Scan(&unread, &body)
	if want := "2 open asks from " + fixture.agentName; unread != 1 || body != want {
		test.Fatalf("unread=%d body=%q, want one item %q", unread, body, want)
	}
	if candidates := repingCandidates(test); candidates[uuidToString(third.ID)] || candidates[uuidToString(second.ID)] {
		test.Fatalf("the re-ping covers the remaining list, got %v", candidates)
	}
}

func TestDecisionPingAfterHeadItemReadPingsAgain(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	fixture.askAs(test, "Approve the first change?")
	dbfx.Exec(test, `UPDATE inbox_item SET read = true WHERE issue_id = $1`, fixture.issueID)
	fixture.askAs(test, "Approve the second change?")
	if n := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND read = false`, fixture.issueID); n != 1 {
		test.Fatalf("a person who read the ping must hear about the new ask, unread = %d", n)
	}
}
