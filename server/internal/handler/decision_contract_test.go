package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDecisionStructuredAskMissingFields(test *testing.T) {
	for _, missing := range []string{"agent_did", "decision", "options", "recommendation", "default_if_no_reply", "source_issue_id", "source_comment_id"} {
		test.Run(missing, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			content, err := json.Marshal(fixture.structuredAsk())
			if err != nil {
				test.Fatal(err)
			}
			var ask map[string]any
			if err := json.Unmarshal(content, &ask); err != nil {
				test.Fatal(err)
			}
			delete(ask, missing)
			request := fixture.apiRequest()
			request.Body = ioBody(mustDecisionJSON(test, map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "escalation_class": "reversible", "ask": ask}))
			testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusBadRequest)
			fixture.assertCounts(test, 0)
		})
	}
}

func mustDecisionJSON(test *testing.T, value any) string {
	test.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return string(content)
}

func TestDecisionStructuredAskSourceBoundary(test *testing.T) {
	for _, mutation := range []string{"deleted", "wrong_issue", "wrong_workspace", "source_issue_mismatch"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			ask := fixture.structuredAsk()
			want := http.StatusNotFound
			switch mutation {
			case "deleted":
				ask.SourceCommentID = dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": fixture.humanID, "content": "Deleted source", "deleted_at": testutil.Raw("clock_timestamp()")})
			case "wrong_issue":
				ask.SourceCommentID = dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": dbfx.Issue(test, "Other issue"), "author_type": "member", "author_id": fixture.humanID, "content": "Unrelated source"})
			case "wrong_workspace":
				workspaceID := dbfx.Workspace(test, "Other source workspace", "other-source-"+fixture.issueID)
				ask.SourceCommentID = dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": workspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": fixture.humanID, "content": "Foreign source"})
			case "source_issue_mismatch":
				ask.SourceIssueID = dbfx.Issue(test, "Mismatched source")
				want = http.StatusBadRequest
			}
			request := fixture.apiRequest()
			request.Body = ioBody(mustDecisionJSON(test, map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "escalation_class": "reversible", "ask": ask}))
			testutil.Call(test, testHandler.RequestDecision, request).Want(want)
			fixture.assertCounts(test, 0)
		})
	}
}

func TestDecisionStructuredAskRetryKeepsOriginalProvenance(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	var first, replay db.DecisionRequest
	testutil.Call(test, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated).JSON(&first)
	dbfx.Exec(test, `UPDATE comment SET content = 'Changed later.' WHERE id = $1`, fixture.tagID)
	testutil.Call(test, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusOK).JSON(&replay)
	if replay.ID != first.ID || replay.SourceContentSnapshot != fixture.mention() || replay.SourceCommentID != parseUUID(fixture.tagID) || replay.Ask != first.Ask {
		test.Fatalf("retry changed immutable provenance: first=%+v replay=%+v", first, replay)
	}
	fixture.assertCounts(test, 1)
	if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); count != 1 {
		test.Fatalf("retry created %d inbox pings", count)
	}
}

func TestDecisionStructuredAskConcurrentRetry(test *testing.T) {
	for _, singleCall := range []bool{false, true} {
		test.Run(map[bool]string{false: "intent", true: "single_call"}[singleCall], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
			if singleCall {
				dbfx.Exec(test, `UPDATE decision_intent SET closed_at = clock_timestamp(), close_reason = 'superseded', sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
			}
			input := fixture.input()
			input.SourceCommentID, input.Ask = parseUUID(fixture.tagID), fixture.structuredAsk().Content(handlerTestWorkspaceSlug)
			if singleCall {
				input.HumanUserID = parseUUID(fixture.humanID)
			}
			outcomes := make(chan requestCoreOutcome, 2)
			start := make(chan struct{})
			var workers sync.WaitGroup
			workers.Add(2)
			for range 2 {
				go func() {
					defer workers.Done()
					<-start
					result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
					outcomes <- requestCoreOutcome{result: result, err: err}
				}()
			}
			close(start)
			workers.Wait()
			close(outcomes)
			var requestID string
			for outcome := range outcomes {
				if outcome.err != nil {
					test.Fatal(outcome.err)
				}
				if requestID != "" && requestID != uuidToString(outcome.result.Request.ID) {
					test.Fatal("concurrent retry created another request")
				}
				requestID = uuidToString(outcome.result.Request.ID)
			}
			fixture.assertCounts(test, 1)
		})
	}
}

func TestDecisionWithdrawRoundTripAndRetry(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	call := func(reason string) *httptest.ResponseRecorder {
		request := withURLParam(fixture.request(), "id", uuidToString(fixture.decision.ID))
		request.Method = http.MethodPost
		request.Body = ioBody(mustDecisionJSON(test, map[string]any{"reason": reason}))
		return testutil.Call(test, testHandler.WithdrawDecision, request).Want(http.StatusOK).ResponseRecorder
	}
	var first, replay db.DecisionRequest
	if err := json.Unmarshal(call("Superseded.").Body.Bytes(), &first); err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(call("Superseded.").Body.Bytes(), &replay); err != nil {
		test.Fatal(err)
	}
	if first.ID != replay.ID || first.Status != "cancelled" || first.CancelReason.String != "agent_withdrawn" || first.WithdrawReason != "Superseded." || !first.WithdrawnAt.Valid || first.Ask != fixture.decision.Ask || first.WithdrawnByTaskID != parseUUID(fixture.taskID) {
		test.Fatalf("wrong withdrawn state: %+v %+v", first, replay)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, first.ID); count != 1 {
		test.Fatalf("retry created %d cancellation comments", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, first.ID); count != 0 {
		test.Fatal("withdrawing an ask created an answer")
	}
}

func TestDecisionWithdrawAuthorization(test *testing.T) {
	for _, mutation := range []string{"other_agent", "other_human", "stopped_task", "wrong_workspace", "empty_reason", "answered", "changed_reason", "human"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			request := withURLParam(fixture.request(), "id", uuidToString(fixture.decision.ID))
			request.Method = http.MethodPost
			reason := "Superseded."
			want := http.StatusForbidden
			switch mutation {
			case "other_agent":
				request.Header.Set("X-Agent-ID", createHandlerTestAgent(test, "Other withdrawer", []byte("[]")))
			case "other_human":
				request.Header.Set("X-Actor-Source", "interactive_jwt")
			case "stopped_task":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
			case "wrong_workspace":
				workspaceID := dbfx.Workspace(test, "Withdrawal other workspace", "withdraw-other-"+fixture.issueID)
				dbfx.Insert(test, "member", testutil.Cols{"workspace_id": workspaceID, "user_id": testUserID, "role": "member"})
				request.Header.Set("X-Workspace-ID", workspaceID)
				want = http.StatusNotFound
			case "empty_reason":
				reason, want = " \n", http.StatusBadRequest
			case "answered":
				if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
					test.Fatal(err)
				}
				want = http.StatusConflict
			case "changed_reason":
				if _, err := testHandler.withdrawDecisionRequestTx(request, fixture.decision, "Original reason."); err != nil {
					test.Fatal(err)
				}
				want = http.StatusConflict
			case "human":
				request.Header.Set("X-Actor-Source", "interactive_jwt")
				request.Header.Set("X-User-ID", fixture.humanID)
				want = http.StatusOK
			}
			request.Body = ioBody(mustDecisionJSON(test, map[string]any{"reason": reason}))
			testutil.Call(test, testHandler.WithdrawDecision, request).Want(want)
		})
	}
}

func TestDecisionAnswerVersusWithdrawRace(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	var answer recordCoreOutcome
	var withdrawn decisionRequestResult
	var withdrawErr error
	var workers sync.WaitGroup
	workers.Add(2)
	start := make(chan struct{})
	go func() {
		defer workers.Done()
		<-start
		answer.result, answer.err = testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	}()
	go func() {
		defer workers.Done()
		<-start
		withdrawn, withdrawErr = retryDecisionTx(fixture.request(), func() (decisionRequestResult, error) {
			return testHandler.withdrawDecisionRequestTx(fixture.request(), fixture.decision, "No longer needed.")
		})
	}()
	close(start)
	workers.Wait()
	if (answer.err == nil) == (withdrawErr == nil) {
		test.Fatalf("exactly one terminal action must succeed: answer=%v withdrawal=%v", answer.err, withdrawErr)
	}
	loser := answer.err
	if loser == nil {
		loser = withdrawErr
	}
	var refusal decisionIntentRefusal
	if !errors.As(loser, &refusal) || refusal.status != http.StatusConflict {
		test.Fatalf("loser did not observe terminal state: %v", loser)
	}
	stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID})
	if err != nil {
		test.Fatal(err)
	}
	if answer.err == nil && stored.Status != "answered" || withdrawErr == nil && (stored.Status != "cancelled" || !withdrawn.Request.WithdrawnAt.Valid) {
		test.Fatalf("terminal state disagrees with winning transaction: %+v", stored)
	}
}

func TestDecisionAskBlockingRulesReturn400(test *testing.T) {
	const commentUUID = "01a17cda-b791-7da5-94fc-06fdec20e4ed"
	option := func(ask map[string]any, index int) map[string]any {
		return ask["options"].([]any)[index].(map[string]any)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"CHE-2204 multi-question", func(ask map[string]any) {
			ask["decision"] = "Answer 1 to 5. 1. Run budgets: close CHE-2164. 2. CHE-1532: add the secrets. 3. CHE-2007: run the packet. 4. CHE-2166: lane or waive. 5. Overnight queue."
		}, "ask.decision asks more than one question; split into one request per question"},
		{"inline list in a label", func(ask map[string]any) { option(ask, 0)["label"] = "1. KEEP 2. WAIVE" },
			"ask.options[0].label has an inline numbered list; put each item on its own line as a Markdown list"},
		{"bare UUID in cost_or_time", func(ask map[string]any) { option(ask, 1)["cost_or_time"] = "Wait for " + commentUUID },
			"ask.options[1].cost_or_time has a bare ID; link it as [what it is](link)"},
		{"ID-only agent_did", func(ask map[string]any) { ask["agent_did"] = "75eed3bd-33c6-42f5-b612-cf0f03b8879a" },
			"ask.agent_did must say in at least one plain sentence what the agent did; an ID alone is not enough"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			ask := fixture.newShapeAsk()
			tc.mutate(ask)
			response := testutil.Call(test, testHandler.RequestDecision, fixture.askRequest(test, ask)).Want(http.StatusBadRequest)
			if got := strings.TrimSpace(response.Body.String()); !strings.Contains(got, tc.want) {
				test.Fatalf("body = %s, want it to contain %q", got, tc.want)
			}
			fixture.assertCounts(test, 0)
		})
	}
}
