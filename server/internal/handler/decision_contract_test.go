package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/decisiontx"
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

func TestDecisionStructuredAskValidationHTTPContract(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(*decisiontx.Ask)
		want   string
	}{
		{
			name: "multi-question decision",
			mutate: func(ask *decisiontx.Ask) {
				ask.Decision = "Answer 1 to 5. 1. Run budgets (spend): close CHE-2164 (its PR #432 merged), and for CHE-1192 choose 1 raise the budget by 3 runs or 2 merge sub2api #71 on Luna's PASS. 2. CHE-1532: add the 3 multica-dotfiles secrets MULTICA_TOKEN, MULTICA_SERVER_URL, MULTICA_WORKSPACE_ID (5 min). 3. CHE-2007: run the packet on X99 with attachment 01a17b40, dry run then --apply (10 min). 4. CHE-2166: lane or waive for the C00 read-back. 5. Overnight queue items 1 to 5 from the brief (CHE-2127 fix, CHE-2042 review, CHE-2109 test fix, C00 read-back after your deploy approval, stale-lane triage)."
			},
			want: "ask.decision asks more than one question; split into one request per question",
		},
		{
			name:   "inline numbered option label",
			mutate: func(ask *decisiontx.Ask) { ask.Options[0].Label = "1. first step 2. second step" },
			want:   "ask.options[0].label has an inline numbered list; put each item on its own line as a Markdown list",
		},
		{
			name:   "bare UUID in option cost",
			mutate: func(ask *decisiontx.Ask) { ask.Options[1].CostOrTime = "Wait for 01a17cda-b791-7da5-94fc-06fdec20e4ed" },
			want:   "ask.options[1].cost_or_time has a bare ID; link it as [what it is](link)",
		},
		{
			name:   "ID-only agent did",
			mutate: func(ask *decisiontx.Ask) { ask.AgentDid = "75eed3bd-33c6-42f5-b612-cf0f03b8879a" },
			want:   "ask.agent_did must say in at least one plain sentence what the agent did; an ID alone is not enough",
		},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newIntentFixture(test)
			tagID := dbfx.Comment(test, fixture.issueID, fixture.mention(), testutil.Cols{
				"author_type": "agent", "author_id": fixture.agentID, "source_task_id": fixture.taskID,
			})
			intentID := dbfx.Insert(test, "decision_intent", testutil.Cols{
				"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "task_id": fixture.taskID,
				"agent_id": fixture.agentID, "comment_id": tagID, "expires_at": time.Now().Add(3 * time.Minute),
			})
			dbfx.Exec(test, `UPDATE comment SET decision_intent_id = $1 WHERE id = $2`, intentID, tagID)
			dbfx.Exec(test, `INSERT INTO decision_intent_target (decision_intent_id, human_user_id) VALUES ($1, $2)`, intentID, fixture.humanID)
			sessionID := dbfx.Insert(test, "chat_session", testutil.Cols{
				"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "Validation contract",
			})
			var issueWorkspaceID string
			dbfx.QueryRow(test, `SELECT workspace_id::text FROM issue WHERE id = $1`, fixture.issueID).Scan(&issueWorkspaceID)
			if _, err := testHandler.Queries.GetIssueInWorkspace(context.Background(), db.GetIssueInWorkspaceParams{
				ID: parseUUID(fixture.issueID), WorkspaceID: parseUUID(issueWorkspaceID),
			}); err != nil {
				test.Fatalf("test fixture issue is not in its stored workspace: %v", err)
			}
			ask := structuredDecisionAsk(fixture.issueID, tagID, "Approve the reversible change?")
			tc.mutate(&ask)
			request := newRequest(http.MethodPost, "/api/decisions", map[string]any{
				"issue_id": fixture.issueID, "session_id": sessionID,
				"escalation_class": "reversible", "ask": ask,
			})
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
			request.Header.Set("X-Workspace-ID", issueWorkspaceID)
			issueResponse := httptest.NewRecorder()
			if _, ok := testHandler.loadIssueForUser(issueResponse, request, fixture.issueID); !ok {
				test.Fatalf("test fixture issue is not visible: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
			}
			response := testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusBadRequest).ResponseRecorder
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				test.Fatal(err)
			}
			if body.Error != tc.want {
				test.Fatalf("error = %q, want %q", body.Error, tc.want)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); count != 0 {
				test.Fatalf("validation created %d decision requests", count)
			}
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
			input.SourceCommentID, input.Ask = parseUUID(fixture.tagID), fixture.structuredAsk().Content()
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
