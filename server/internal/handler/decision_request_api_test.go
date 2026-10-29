package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (fixture requestCoreFixture) apiRequest() *http.Request {
	request := newRequest(http.MethodPost, "/api/decisions", map[string]any{
		"issue_id": fixture.issueID, "session_id": fixture.sessionID,
		"ask": fixture.structuredAsk(), "escalation_class": fixture.input().EscalationClass,
	})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return request
}

func structuredDecisionAsk(issueID, commentID, decision string) decisiontx.Ask {
	return decisiontx.Ask{ContextELI10: "A change is ready. You decide when it ships.", AgentDid: "Ran scoped checks.", Decision: decision, StakesIfWrong: "A bad change could reach users.", Options: []decisiontx.AskOption{testAskOption("Proceed"), testAskOption("Hold")}, Recommendation: "Hold", DefaultIfNoReply: "Do not proceed.", SourceIssueID: issueID, SourceCommentID: commentID}
}

func testAskOption(label string) decisiontx.AskOption {
	return decisiontx.AskOption{Label: label, WhatHappens: label + " happens.", Pros: []string{"A pro"}, Cons: []string{"A con"}, CostOrTime: "No extra time"}
}

func (fixture requestCoreFixture) structuredAsk() decisiontx.Ask {
	return structuredDecisionAsk(fixture.issueID, fixture.tagID, fixture.input().Ask)
}

func TestDecisionRequestAPIRoundTrip(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	older := time.Now().Add(-time.Hour)
	dbfx.Exec(test, `UPDATE chat_session SET updated_at = $1 WHERE id = $2`, older, fixture.sessionID)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	var created map[string]any
	var inboxEvents, commentEvents, chatEvents int
	testHandler.Bus.Subscribe(protocol.EventInboxNew, func(event events.Event) {
		if event.ActorID == fixture.agentID {
			inboxEvents++
		}
	})
	testHandler.Bus.Subscribe(protocol.EventCommentCreated, func(event events.Event) {
		if event.ActorID == fixture.agentID {
			payload := event.Payload.(map[string]any)
			targets, ok := payload["decision_intent_targets"].([]string)
			if !ok || len(targets) != 1 || targets[0] != fixture.humanID {
				test.Fatalf("ask event did not suppress the receipt-owned target: %+v", payload)
			}
			commentEvents++
		}
	})
	testHandler.Bus.Subscribe(protocol.EventChatMessage, func(event events.Event) {
		if event.ActorID == fixture.agentID {
			chatEvents++
		}
	})
	testutil.Call(test, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated).JSON(&created)
	workspace, err := testHandler.Queries.GetWorkspace(context.Background(), fixture.issue.WorkspaceID)
	if err != nil {
		test.Fatal(err)
	}
	requestID, ok := created["id"].(string)
	if !ok || created["ask"] != fixture.structuredAsk().Content(workspace.Slug) || created["status"] != "open" || created["human_user_id"] != fixture.humanID || created["ping_notified_at"] == nil {
		test.Fatalf("wrong request response: %+v", created)
	}
	wantCommentLink := "[Comment](" + "/" + workspace.Slug + "/issues/" + fixture.issueID + "#comment-" + fixture.tagID + ")"
	if !strings.Contains(created["ask"].(string), wantCommentLink) {
		test.Fatalf("decision request ask has wrong comment link: %s", created["ask"])
	}
	fixture.assertCounts(test, 1)
	var updated time.Time
	dbfx.QueryRow(test, `SELECT updated_at FROM chat_session WHERE id = $1`, fixture.sessionID).Scan(&updated)
	if !updated.After(older) {
		test.Fatal("ping did not touch the session's activity timestamp")
	}
	if inboxEvents != 1 || commentEvents != 1 || chatEvents != 1 {
		test.Fatalf("post-commit events: inbox=%d comment=%d chat=%d", inboxEvents, commentEvents, chatEvents)
	}
	var details map[string]string
	var rawDetails []byte
	dbfx.QueryRow(test, `SELECT details FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2 AND type = 'mentioned'`, fixture.issueID, fixture.humanID).Scan(&rawDetails)
	if err := json.Unmarshal(rawDetails, &details); err != nil || details["decision_request_id"] != requestID || details["comment_id"] != created["issue_comment_id"] {
		test.Fatalf("wrong inbox anchor: %s error=%v", rawDetails, err)
	}
	var fetched map[string]any
	getRequest := withURLParam(newRequest(http.MethodGet, "/api/decisions/"+requestID, nil), "id", requestID)
	testutil.Call(test, testHandler.GetDecision, getRequest).Want(http.StatusOK).JSON(&fetched)
	if fetched["id"] != requestID || fetched["ask"] != created["ask"] {
		test.Fatalf("wrong get response: %+v", fetched)
	}
	var listed []map[string]any
	testutil.Call(test, testHandler.ListDecisions, newRequest(http.MethodGet, "/api/decisions?issue="+fixture.issueID, nil)).Want(http.StatusOK).JSON(&listed)
	if len(listed) != 1 || listed[0]["id"] != requestID {
		test.Fatalf("wrong list response: %+v", listed)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, fixture.sessionID); count != 0 {
		test.Fatalf("ping started %d DM turns", count)
	}
}

func TestDecisionRequestAPIAskContentLimit(test *testing.T) {
	for _, size := range []int{maxCommentContentBytes, maxCommentContentBytes + 1} {
		test.Run(map[bool]string{false: "at_limit", true: "over_limit"}[size > maxCommentContentBytes], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
			ask := fixture.structuredAsk()
			workspace, err := testHandler.Queries.GetWorkspace(context.Background(), fixture.issue.WorkspaceID)
			if err != nil {
				test.Fatal(err)
			}
			ask.Decision = strings.Repeat("a", size-len(ask.Content(workspace.Slug))+len(ask.Decision))
			body := map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "ask": ask, "escalation_class": "reversible"}
			request := newRequest(http.MethodPost, "/api/decisions", body)
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
			if size > maxCommentContentBytes {
				testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusBadRequest)
				fixture.assertCounts(test, 0)
			} else {
				testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusCreated)
				fixture.assertCounts(test, 1)
			}
		})
	}
}

func TestDecisionRequestAPIReadAuthorization(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	requestID := uuidToString(result.Request.ID)
	outsider := dbfx.User(test, "Decision outsider", "decision-outsider@example.test")
	otherWorkspace := dbfx.Workspace(test, "Decision other workspace", "decision-other-"+fixture.issueID)
	for _, operation := range []string{"get", "list"} {
		for _, denied := range []string{"outsider", "wrong workspace", "anonymous"} {
			test.Run(operation+"/"+denied, func(test *testing.T) {
				request := newRequest(http.MethodGet, "/api/decisions?issue="+fixture.issueID, nil)
				handle := testHandler.ListDecisions
				if operation == "get" {
					request = withURLParam(request, "id", requestID)
					handle = testHandler.GetDecision
				}
				switch denied {
				case "outsider":
					request.Header.Set("X-User-ID", outsider)
				case "wrong workspace":
					request.Header.Set("X-Workspace-ID", otherWorkspace)
				case "anonymous":
					request.Header.Del("X-User-ID")
				}
				testutil.Call(test, handle, request).WantOneOf(http.StatusForbidden, http.StatusNotFound, http.StatusUnauthorized)
			})
		}
	}
}

func TestDecisionRequestAPIRefusalHasNoSideEffects(test *testing.T) {
	for _, source := range []string{"interactive_jwt", "cli_jwt", "pat", "cloud_pat", ""} {
		test.Run(source, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			request := fixture.apiRequest()
			request.Header.Set("X-Actor-Source", source)
			testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusForbidden)
			fixture.assertCounts(test, 0)
			if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); count != 0 {
				test.Fatalf("refusal created %d inbox items", count)
			}
		})
	}
}

func TestDecisionPingReceiptRetryAndCancellation(test *testing.T) {
	for _, cancelled := range []bool{false, true} {
		test.Run(map[bool]string{false: "open", true: "cancelled"}[cancelled], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
			result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
			if err != nil {
				test.Fatal(err)
			}
			if cancelled {
				dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'issue_closed' WHERE id = $1`, result.Request.ID)
			}
			for attempt := 0; attempt < 3; attempt++ {
				item, err := decisiontx.NotifyPing(context.Background(), testPool, testHandler.Queries, result.Request.WorkspaceID, result.Request.ID)
				if err != nil || ((attempt > 0 || cancelled) && item != nil) {
					test.Fatalf("retry %d: item=%+v error=%v", attempt, item, err)
				}
			}
			wanted := 1
			if cancelled {
				wanted = 0
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); count != wanted {
				test.Fatalf("inbox count=%d want=%d", count, wanted)
			}
			stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: result.Request.WorkspaceID, ID: result.Request.ID})
			if err != nil || !stored.PingNotifiedAt.Valid {
				test.Fatalf("missing durable receipt: %+v error=%v", stored, err)
			}
			pending, err := testHandler.Queries.ListDecisionPingRetries(context.Background(), 100)
			if err != nil {
				test.Fatal(err)
			}
			for _, request := range pending {
				if request.ID == result.Request.ID {
					test.Fatal("received ping remains in worker retry query")
				}
			}
		})
	}
}

func TestDecisionPingReceiptRollbackAndRace(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pending, err := testHandler.Queries.ListDecisionPingRetries(ctx, 100)
	if err != nil || len(pending) != 1 || pending[0].ID != result.Request.ID {
		test.Fatalf("missing retry candidate: %+v error=%v", pending, err)
	}
	failure := &pgconn.PgError{Code: "23514", Message: "receipt failure after inbox insertion"}
	broken := &requestCoreStarter{base: testPool, query: "-- name: MarkDecisionPingNotified", fail: failure}
	item, err := decisiontx.NotifyPing(ctx, broken, testHandler.Queries, result.Request.WorkspaceID, result.Request.ID)
	if !errors.Is(err, failure) || item != nil || dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID) != 0 {
		test.Fatalf("failed receipt escaped rollback: item=%+v error=%v", item, err)
	}
	inserted := make(chan struct{})
	firstPID := make(chan int, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	paused := &requestCoreStarter{base: testPool, query: "-- name: MarkDecisionPingNotified", before: func(transaction pgx.Tx) error {
		var count int
		if err := transaction.QueryRow(ctx, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID).Scan(&count); err != nil || count != 1 {
			return errors.New("receipt was not reached after exactly one insertion")
		}
		var backendPID int
		if err := transaction.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			return err
		}
		firstPID <- backendPID
		close(inserted)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	errorsFound := make(chan error, 2)
	go func() {
		_, err := decisiontx.NotifyPing(ctx, paused, testHandler.Queries, result.Request.WorkspaceID, result.Request.ID)
		errorsFound <- err
	}()
	select {
	case <-inserted:
	case <-ctx.Done():
		test.Fatal("first notifier never reached receipt")
	}
	go func() {
		_, err := decisiontx.NotifyPing(ctx, testPool, testHandler.Queries, result.Request.WorkspaceID, result.Request.ID)
		errorsFound <- err
	}()
	if !waitForWaiterBlockedBy(test, <-firstPID, 3*time.Second) {
		test.Fatal("second notifier did not wait behind the first insertion/receipt transaction")
	}
	releaseOnce.Do(func() { close(release) })
	for attempt := 0; attempt < 2; attempt++ {
		if err := <-errorsFound; err != nil {
			test.Fatal(err)
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); count != 1 {
		test.Fatalf("racing notifiers created %d items", count)
	}
}

func TestDecisionRequestAPIReceiptFailureIsRetryableAfterCommit(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	broken := &requestCoreStarter{base: testPool, query: "-- name: MarkDecisionPingNotified", fail: errors.New("injected receipt failure")}
	useRequestCoreStarter(test, broken)
	var created db.DecisionRequest
	testutil.Call(test, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated).JSON(&created)
	fixture.assertCounts(test, 1)
	if created.PingNotifiedAt.Valid || dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID) != 0 {
		test.Fatal("failed post-commit receipt left an inbox item or marked success")
	}
	item, err := decisiontx.NotifyPing(context.Background(), testPool, testHandler.Queries, created.WorkspaceID, created.ID)
	if err != nil || item == nil || dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID) != 1 {
		test.Fatalf("worker retry failed: item=%+v error=%v", item, err)
	}
}

func TestDecisionRequestAPIMalformedInput(test *testing.T) {
	for _, malformed := range []string{"json", "session", "ask", "class"} {
		test.Run(malformed, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			body := map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "ask": fixture.structuredAsk(), "escalation_class": "reversible"}
			var input any = body
			switch malformed {
			case "json":
				input = "{"
			case "session":
				body["session_id"] = "not-a-uuid"
			case "ask":
				body["ask"] = " \n "
			case "class":
				body["escalation_class"] = "anything"
			}
			request := newRequest(http.MethodPost, "/api/decisions", input)
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
			testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusBadRequest)
			fixture.assertCounts(test, 0)
		})
	}
}

// A human who left the workspace cannot receive the ping. The worker must
// record the ping as handled instead of retrying it every cycle.
func TestDecisionPingToFormerMemberIsRecordedNotRetried(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, fixture.humanID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	item, err := decisiontx.NotifyPing(ctx, testPool, testHandler.Queries, result.Request.WorkspaceID, result.Request.ID)
	if err != nil || item != nil {
		test.Fatalf("ping to a former member: item=%+v error=%v, want no item and no error", item, err)
	}
	pending, err := testHandler.Queries.ListDecisionPingRetries(ctx, 100)
	if err != nil {
		test.Fatal(err)
	}
	for _, request := range pending {
		if request.ID == result.Request.ID {
			test.Fatal("ping for a former member is still queued for retry")
		}
	}
}
