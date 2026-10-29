package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const cardOptions = `[{"label":"Option A","what_happens":"A happens"},{"label":"Option B","what_happens":"B happens"}]`

func newCardFixture(test *testing.T) askedRecordFixture {
	test.Helper()
	fixture := newAskedRecordFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb WHERE id = $1`, fixture.decision.ID, cardOptions)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
	return fixture
}

func (fixture askedRecordFixture) answerOnCard(input decisionCardAnswerInput) (decisionRecordResult, error) {
	return testHandler.answerDecisionRequestTx(fixture.request(), fixture.issue.WorkspaceID, fixture.decision.ID, parseUUID(fixture.humanID), input)
}

func TestCardOptionAnswerRecordsOnThatCardWithoutDMRun(test *testing.T) {
	resetDecisionLease(test)
	fixture := newCardFixture(test)
	runsBefore := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, fixture.sessionID)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
	if err != nil {
		test.Fatal(err)
	}
	if result.Request.Status != "answered" || result.Answer.AnsweredByTaskID.Valid || !result.Answer.WakeAt.Valid || result.Answer.ContentSnapshot != "Option B" {
		test.Fatalf("wrong answered state: %+v %+v", result.Request, result.Answer)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, fixture.issueID); count != 1 {
		test.Fatalf("answers on the issue = %d, want 1", count)
	}
	if result.Comment.DecisionCommentKind.String != "quote" || result.Comment.ParentID != parseUUID(fixture.parentID) || !strings.Contains(result.Comment.Content, "> Option B") || !strings.Contains(result.Comment.Content, "/chat?session="+fixture.sessionID) {
		test.Fatalf("quote lacks the option or the card link: %+v", result.Comment)
	}
	var role, sentVia string
	var taskless bool
	dbfx.QueryRow(test, `SELECT role, sent_via, task_id IS NULL FROM chat_message WHERE id = $1`, result.Answer.ChatMessageIds[0]).Scan(&role, &sentVia, &taskless)
	if role != "user" || sentVia != "interactive_jwt" || !taskless {
		test.Fatalf("answer message = %s/%s/taskless=%v", role, sentVia, taskless)
	}
	if runs := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, fixture.sessionID); runs != runsBefore {
		test.Fatalf("DM runs went %d -> %d", runsBefore, runs)
	}

	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	_, runner := newDecisionRunnerFixture(test)
	for range 2 {
		decisionPass(test, runner)
		if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running') AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fixture.issueID, fixture.agentID, result.Comment.ID); count != 1 {
			test.Fatalf("assignee wake runs = %d, want 1", count)
		}
	}
	if runs := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status IN ('queued', 'running')`, fixture.sessionID); runs != 0 {
		test.Fatalf("the wake started %d DM runs", runs)
	}
}

func TestCardTextAnswerRecordsTheText(test *testing.T) {
	fixture := newCardFixture(test)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Text: "  Neither.\nShip C instead.  "})
	if err != nil {
		test.Fatal(err)
	}
	if result.Answer.ContentSnapshot != "Neither.\nShip C instead." || !strings.Contains(result.Comment.Content, "> Neither.\n> Ship C instead.") {
		test.Fatalf("text not recorded: %+v", result.Answer)
	}
}

func TestCardOptionPlusTextRecordsBoth(test *testing.T) {
	both, err := newCardFixture(test).answerOnCard(decisionCardAnswerInput{Option: "Option A", Text: "but wait a day"})
	if err != nil || both.Answer.ContentSnapshot != "Option A\n\nbut wait a day" {
		test.Fatalf("option plus text = %+v, %v", both.Answer, err)
	}
}

func TestCardAnswerRefusals(test *testing.T) {
	fixture := newCardFixture(test)
	for name, input := range map[string]decisionCardAnswerInput{
		"empty":          {},
		"blank text":     {Text: " \n "},
		"unknown option": {Option: "Option C"},
		"too long":       {Text: strings.Repeat("x", maxDecisionCardTextBytes+1)},
	} {
		if _, err := fixture.answerOnCard(input); err == nil {
			test.Fatalf("%s was accepted", name)
		}
	}
	if _, err := testHandler.answerDecisionRequestTx(fixture.request(), fixture.issue.WorkspaceID, fixture.decision.ID, parseUUID(testUserID), decisionCardAnswerInput{Option: "Option A"}); err == nil && testUserID != fixture.humanID {
		test.Fatal("another member answered the card")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID); count != 0 {
		test.Fatalf("refusals left %d answers", count)
	}
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"}); err == nil {
		test.Fatal("an answered card took a second answer")
	}
}

// The card shows a draft back with "nothing was posted" when it loses the race
// to another answer, so the refusal must be a 409 that leaves no trace.
func TestCardAnswerLosingTheRacePostsNothing(test *testing.T) {
	fixture := newCardFixture(test)
	first, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"})
	if err != nil {
		test.Fatal(err)
	}
	counts := func() [3]int {
		return [3]int{
			dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID),
			dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID),
			dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID),
		}
	}
	before := counts()
	_, err = fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B", Text: "late draft"})
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || refusal.status != http.StatusConflict {
		test.Fatalf("losing answer = %v, want a 409 refusal", err)
	}
	if after := counts(); after != before {
		test.Fatalf("a refused answer left rows: messages/comments/answers %v -> %v", before, after)
	}
	var snapshot string
	dbfx.QueryRow(test, `SELECT content_snapshot FROM decision_answer WHERE id = $1`, first.Answer.ID).Scan(&snapshot)
	if snapshot != "Option A" {
		test.Fatalf("the first answer became %q", snapshot)
	}
}

func TestCardAnswerCanBeDisclaimed(test *testing.T) {
	resetDecisionLease(test)
	fixture := newCardFixture(test)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"})
	if err != nil {
		test.Fatal(err)
	}
	withdrawn, err := testHandler.disclaimDecisionAnswerTx(fixture.request(), fixture.issue.WorkspaceID, result.Answer.ID, parseUUID(fixture.humanID))
	if err != nil {
		test.Fatal(err)
	}
	stored, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: result.Answer.ID})
	if err != nil || !stored.DisclaimedAt.Valid || !withdrawn.Answer.DisclaimedAt.Valid {
		test.Fatalf("card answer not disclaimed: %+v %v", stored, err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'running') AND trigger_comment_id = $2`, fixture.issueID, result.Comment.ID); count != 0 {
		test.Fatalf("a disclaimed card answer woke the assignee %d times", count)
	}
}

func TestDecisionRequestKeepsCardOptions(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	input := fixture.input()
	input.Options = []decisiontx.AskOption{{Label: "Ship now", WhatHappens: "release"}, {Label: "Wait", WhatHappens: "hold"}}
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	var stored []decisiontx.AskOption
	if err := json.Unmarshal(result.Request.Options, &stored); err != nil || len(stored) != 2 || stored[0].Label != "Ship now" || stored[1].WhatHappens != "hold" {
		test.Fatalf("options not stored: %s %v", result.Request.Options, err)
	}
	cards := testHandler.decisionRequestsForMessages(context.Background(), testWorkspaceID, []db.ChatMessage{{ID: result.Message.ID, ChatSessionID: result.Message.ChatSessionID}})
	if got := cards[uuidToString(result.Message.ID)]; len(got) != 1 || len(got[0].Options) != 2 || got[0].Options[0].Label != "Ship now" {
		test.Fatalf("card JSON lacks options: %+v", got)
	}
}

func TestCardAnswerEndpointRequiresTheWebHuman(test *testing.T) {
	fixture := newCardFixture(test)
	request := newRequest(http.MethodPost, "/api/decision-requests/"+uuidToString(fixture.decision.ID)+"/answer", map[string]any{"option": "Option A"})
	request = withURLParam(request, "id", uuidToString(fixture.decision.ID))
	request.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(test, testHandler.AnswerDecisionRequest, request).Want(http.StatusForbidden)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID); count != 0 {
		test.Fatalf("an agent token recorded %d answers", count)
	}
}

// A composer message must not record anything, and the run must see every open
// card with what it needs to name one.
func TestComposerMessageCarriesOpenCardsAndRecordsNothing(test *testing.T) {
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(test, ctx, "Card DM")
	ask := decisiontx.Ask{Decision: "Ship Friday?", StakesIfWrong: "customers see a broken page", Recommendation: "Option B",
		Options: []decisiontx.AskOption{{Label: "Option A", WhatHappens: "ship now"}, {Label: "Option B", WhatHappens: "ship Monday"}}}
	var ids []string
	for range 4 {
		requestID, _, _ := seedDMDecision(test, agentID, sessionID, ask.Content(handlerTestWorkspaceSlug), "open", "3 hours")
		dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb WHERE id = $1`, requestID, cardOptions)
		ids = append(ids, requestID)
	}
	sendDirectChat(test, ctx, agentID, sessionID, "B, but only after the freeze")
	carrier := claimForkTask(test, runtimeID, daemonID).ChatDecisionContext
	for _, id := range ids {
		if !strings.Contains(carrier, id) {
			test.Fatalf("carrier lacks open card %s:\n%s", id, carrier)
		}
	}
	for _, want := range []string{"Ship Friday?", "customers see a broken page", "Option B", "A happens", "answer:", "Do not record"} {
		if !strings.Contains(carrier, want) {
			test.Fatalf("carrier lacks %q:\n%s", want, carrier)
		}
	}
	if strings.Contains(carrier, "A short reply answers it") {
		test.Fatalf("carrier still binds a bare reply:\n%s", carrier)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = ANY($1::uuid[])`, ids); count != 0 {
		test.Fatalf("composer message recorded %d answers", count)
	}
}
