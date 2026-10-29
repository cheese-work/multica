package handler

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type cancelMessages struct{ comments, dms []string }

// dmsBefore skips the DM notes that exist before the close, such as an answer's reference.
func readCancelMessages(test *testing.T, issueID, sessionID string, dmsBefore int) cancelMessages {
	test.Helper()
	var messages cancelMessages
	for _, query := range []struct {
		sql  string
		into *[]string
		arg  string
	}{
		{`SELECT content FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'cancel' ORDER BY created_at, id`, &messages.comments, issueID},
		{`SELECT content FROM chat_message WHERE chat_session_id = $1 AND message_kind IN ('decision_reference', 'decision_cancel_notice') ORDER BY created_at, id OFFSET ` + strconv.Itoa(dmsBefore), &messages.dms, sessionID},
	} {
		rows, err := testPool.Query(context.Background(), query.sql, query.arg)
		if err != nil {
			test.Fatal(err)
		}
		for rows.Next() {
			var content string
			if err := rows.Scan(&content); err != nil {
				test.Fatal(err)
			}
			*query.into = append(*query.into, content)
		}
		rows.Close()
	}
	return messages
}

// closeAndCancel closes the issue, runs the worker and returns the DM notes that existed before.
func closeAndCancel(test *testing.T, fixture requestCoreFixture) int {
	test.Helper()
	before := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind IN ('decision_reference', 'decision_cancel_notice')`, fixture.sessionID)
	resetDecisionLease(test)
	issueID := fixture.issueID
	dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, issueID)
	// The activity listener is not running in handler tests; record the status change it would write.
	dbfx.Exec(test, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details) VALUES ($1, $2, 'system', 'status_changed', '{"from":"in_progress","to":"done"}')`, testWorkspaceID, issueID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	decisionPass(test, runner)
	return before
}

// addOpenRequest adds a second open request to a fixture whose intent window is already sealed.
func addOpenRequest(test *testing.T, fixture requestCoreFixture, class string) {
	test.Helper()
	dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "asking_agent_id": fixture.agentID, "human_user_id": fixture.humanID, "chat_session_id": fixture.sessionID, "ask": "Extra ask for " + class, "escalation_class": class, "origin": "asked"})
}

func TestDecisionCancelMessageNamesIssueAndAsk(test *testing.T) {
	test.Setenv("MULTICA_APP_URL", "https://multica.test")
	fixture := newRequestCoreFixture(test, false)
	input := fixture.input()
	input.Ask = "Approve the spend?\nSecond line must not appear."
	input.EscalationClass = "spend"
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input); err != nil {
		test.Fatal(err)
	}
	before := closeAndCancel(test, fixture)
	messages := readCancelMessages(test, fixture.issueID, fixture.sessionID, before)
	if len(messages.comments) != 1 || len(messages.dms) != 1 {
		test.Fatalf("comments=%d dms=%d, want 1 and 1", len(messages.comments), len(messages.dms))
	}
	for _, content := range []string{messages.comments[0], messages.dms[0]} {
		for _, want := range []string{"closed as done", fixture.issue.Title, "spend", "Approve the spend?", "No action needed."} {
			if !strings.Contains(content, want) {
				test.Fatalf("cancel message misses %q:\n%s", want, content)
			}
		}
		if strings.Contains(content, "Second line") || strings.Contains(content, "issue_closed") {
			test.Fatalf("cancel message leaks a later ask line or the raw reason:\n%s", content)
		}
		if !strings.Contains(content, "](https://multica.test/") || !strings.Contains(content, "/issues/") {
			test.Fatalf("cancel message has no issue link:\n%s", content)
		}
	}
}

func TestDecisionCancelMessageMissingAsk(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE decision_request SET ask = '' WHERE issue_id = $1`, fixture.issueID)
	before := closeAndCancel(test, fixture)
	messages := readCancelMessages(test, fixture.issueID, fixture.sessionID, before)
	if len(messages.dms) != 1 || !strings.Contains(messages.dms[0], "ask text unavailable") {
		test.Fatalf("empty ask not reported: %v", messages.dms)
	}
}

func TestDecisionCancelMessageTruncatesAsk(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	input := fixture.input()
	input.Ask = strings.Repeat("é", 300)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input); err != nil {
		test.Fatal(err)
	}
	before := closeAndCancel(test, fixture)
	messages := readCancelMessages(test, fixture.issueID, fixture.sessionID, before)
	if len(messages.dms) != 1 || strings.Contains(messages.dms[0], strings.Repeat("é", 101)) || !strings.Contains(messages.dms[0], strings.Repeat("é", 99)+"…") {
		test.Fatalf("ask not truncated to about 100 characters: %v", messages.dms)
	}
}

func TestDecisionCancelMessageAnsweredRequest(test *testing.T) {
	fixture, _ := readyDecisionDelivery(test)
	before := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID)
	dbfx.Exec(test, `UPDATE chat_session SET updated_at = $1 WHERE id = $2`, time.Now().Add(-time.Hour), fixture.sessionID)
	var updatedBefore time.Time
	dbfx.QueryRow(test, `SELECT updated_at FROM chat_session WHERE id = $1`, fixture.sessionID).Scan(&updatedBefore)
	handler, _ := newDecisionRunnerFixture(test)
	var chatEvents int
	handler.Bus.Subscribe(protocol.EventChatMessage, func(event events.Event) {
		if event.ActorID == fixture.agentID {
			chatEvents++
		}
	})
	dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
	for range 2 {
		if err := cancelDecisionForTest(handler, fixture); err != nil {
			test.Fatal(err)
		}
	}
	messages := readCancelMessages(test, fixture.issueID, fixture.sessionID, 0)
	if len(messages.comments) != 1 {
		test.Fatalf("comments=%d, want 1", len(messages.comments))
	}
	if content := messages.comments[0]; !strings.Contains(content, "answer was recorded") || strings.Contains(content, "cancelled") {
		test.Errorf("answered request must say the answer was recorded, not cancelled:\n%s", content)
	}
	if after := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID); after != before {
		test.Errorf("chat messages=%d, want unchanged count %d", after, before)
	}
	var updatedAfter time.Time
	dbfx.QueryRow(test, `SELECT updated_at FROM chat_session WHERE id = $1`, fixture.sessionID).Scan(&updatedAfter)
	if !updatedAfter.Equal(updatedBefore) {
		test.Error("answered request close touched the chat session")
	}
	if chatEvents != 0 {
		test.Errorf("chat events=%d, want 0", chatEvents)
	}
}

func TestDecisionCancelMessageMixedRequests(test *testing.T) {
	for _, first := range []string{"answered", "open"} {
		test.Run(first+"_first", func(test *testing.T) {
			fixture, _ := readyDecisionDelivery(test)
			addOpenRequest(test, fixture.requestCoreFixture, "spend")
			before := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind IN ('decision_reference', 'decision_cancel_notice')`, fixture.sessionID)
			dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
			dbfx.Exec(test, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details) VALUES ($1, $2, 'system', 'status_changed', '{"from":"in_progress","to":"done"}')`, testWorkspaceID, fixture.issueID)
			requests, err := testHandler.Queries.ListDecisionCancelSiblings(context.Background(), db.ListDecisionCancelSiblingsParams{WorkspaceID: fixture.issue.WorkspaceID, IssueID: fixture.issue.ID, ChatSessionID: parseUUID(fixture.sessionID)})
			if err != nil {
				test.Fatal(err)
			}
			if len(requests) != 2 {
				test.Fatalf("requests=%d, want 2", len(requests))
			}
			if requests[0].Status != first {
				requests[0], requests[1] = requests[1], requests[0]
			}
			for _, request := range requests {
				if _, err := testHandler.CancelDecisionLifecycle(context.Background(), fixture.issue.WorkspaceID, request.ID); err != nil {
					test.Fatal(err)
				}
			}
			messages := readCancelMessages(test, fixture.issueID, fixture.sessionID, before)
			if len(messages.comments) != 2 || len(messages.dms) != 1 {
				test.Fatalf("comments=%d dms=%d, want 2 and 1", len(messages.comments), len(messages.dms))
			}
			if content := messages.dms[0]; strings.Count(content, "\n- ") != 1 || !strings.Contains(content, "Extra ask for spend") || strings.Contains(content, "answer was recorded") {
				test.Errorf("close DM must list only the open request:\n%s", content)
			}
		})
	}
}

func TestDecisionCancelOneDMPerSessionPerClose(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	input := fixture.input()
	input.EscalationClass = "product_direction"
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input); err != nil {
		test.Fatal(err)
	}
	addOpenRequest(test, fixture, "spend")
	addOpenRequest(test, fixture, "spend")
	before := closeAndCancel(test, fixture)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND status = 'cancelled' AND cancel_notice_posted_at IS NOT NULL`, fixture.issueID); count != 3 {
		test.Fatalf("cancelled requests=%d, want 3", count)
	}
	messages := readCancelMessages(test, fixture.issueID, fixture.sessionID, before)
	if len(messages.dms) != 1 || len(messages.comments) != 3 {
		test.Fatalf("dms=%d comments=%d, want 1 and 3", len(messages.dms), len(messages.comments))
	}
	if got := strings.Count(messages.dms[0], "\n- "); got != 3 {
		test.Fatalf("DM lists %d requests, want 3:\n%s", got, messages.dms[0])
	}
}

func TestDecisionCancelReopenedIssueGetsFreshDM(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
		test.Fatal(err)
	}
	before := closeAndCancel(test, fixture)
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issueID)
	addOpenRequest(test, fixture, "spend")
	closeAndCancel(test, fixture)
	if messages := readCancelMessages(test, fixture.issueID, fixture.sessionID, before); len(messages.dms) != 2 {
		test.Fatalf("dms=%d, want one per close", len(messages.dms))
	}
}
