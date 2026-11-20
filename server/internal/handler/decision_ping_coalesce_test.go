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

// enablePush turns on DM push for the fixture's human and gives them one
// device, so the inbox trigger queues a push_delivery row for each ping.
func (fixture requestCoreFixture) enablePush(test *testing.T) {
	test.Helper()
	dbfx.Exec(test, `INSERT INTO notification_channel_preference (workspace_id, user_id, source, channel, enabled) VALUES ($1, $2, 'dm', 'push', true)
		ON CONFLICT (workspace_id, user_id, source, channel) DO UPDATE SET enabled = true`, testWorkspaceID, fixture.humanID)
	dbfx.Exec(test, `INSERT INTO push_subscription (user_id, endpoint, p256dh, auth, session_id, session_valid_until) VALUES ($1, $2, 'k', 'a', 's', now() + interval '1 day')`,
		fixture.humanID, "https://push.example/"+fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM push_subscription WHERE user_id = $1`, fixture.humanID)
	dbfx.Cleanup(test, `DELETE FROM push_delivery WHERE record_id IN (SELECT id FROM inbox_item WHERE issue_id = $1)`, fixture.issueID)
}

// pendingPushes counts push rows still due for the fixture's ping item.
func pendingPushes(test *testing.T, issueID string) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM push_delivery d JOIN inbox_item i ON i.id = d.record_id WHERE i.issue_id = $1 AND d.sent_at IS NULL`, issueID)
}

// CHE-8878: a coalesced ask must still reach the person. The ping item keeps
// one line, but a later ask re-points it at the newest card, moves it to the
// top and queues a push, at most once per 15 minutes.
func TestDecisionPingCoalescedAskRenotifiesOncePerWindow(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	fixture.enablePush(test)
	fixture.askAs(test, "Approve the first change?")
	if n := pendingPushes(test, fixture.issueID); n != 1 {
		test.Fatalf("first ask must queue one push, got %d", n)
	}
	// The first push went out a while ago and the person has not read it.
	dbfx.Exec(test, `UPDATE push_delivery SET sent_at = now() WHERE record_id IN (SELECT id FROM inbox_item WHERE issue_id = $1)`, fixture.issueID)
	dbfx.Exec(test, `UPDATE inbox_item SET created_at = now() - interval '20 minutes', updated_at = now() - interval '20 minutes' WHERE issue_id = $1`, fixture.issueID)
	second := fixture.askAs(test, "Approve the second change?")
	var message string
	var movedUp bool
	dbfx.QueryRow(test, `SELECT chat_message_id::text, updated_at > now() - interval '5 minutes' FROM inbox_item WHERE issue_id = $1`, fixture.issueID).Scan(&message, &movedUp)
	if message != uuidToString(second.PingMessageID) || !movedUp {
		test.Fatalf("item must open the newest card and move to the top: message=%s want=%s movedUp=%v", message, uuidToString(second.PingMessageID), movedUp)
	}
	if n := pendingPushes(test, fixture.issueID); n != 1 {
		test.Fatalf("a coalesced ask after the window must queue one push, got %d", n)
	}
	// A third ask inside the 15 minute window: the line updates, no new push.
	dbfx.Exec(test, `UPDATE push_delivery SET sent_at = now() WHERE record_id IN (SELECT id FROM inbox_item WHERE issue_id = $1)`, fixture.issueID)
	third := fixture.askAs(test, "Approve the third change?")
	var body string
	dbfx.QueryRow(test, `SELECT chat_message_id::text, body FROM inbox_item WHERE issue_id = $1`, fixture.issueID).Scan(&message, &body)
	if want := "3 open asks from " + fixture.agentName; body != want {
		test.Fatalf("body = %q, want %q", body, want)
	}
	if message != uuidToString(second.PingMessageID) || message == uuidToString(third.PingMessageID) {
		test.Fatalf("inside the window the item keeps its card: message=%s", message)
	}
	if n := pendingPushes(test, fixture.issueID); n != 0 {
		test.Fatalf("at most one push per 15 minutes, got %d pending", n)
	}
}

// With a single open ask nothing changes: one item, one push, the ask's own card.
func TestDecisionPingSingleAskKeepsItsPush(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	fixture.enablePush(test)
	first := fixture.askAs(test, "Approve the first change?")
	var message string
	var items int
	dbfx.QueryRow(test, `SELECT count(*), max(chat_message_id::text) FROM inbox_item WHERE issue_id = $1`, fixture.issueID).Scan(&items, &message)
	if items != 1 || message != uuidToString(first.PingMessageID) || pendingPushes(test, fixture.issueID) != 1 {
		test.Fatalf("single ask: items=%d message=%s pushes=%d", items, message, pendingPushes(test, fixture.issueID))
	}
}
