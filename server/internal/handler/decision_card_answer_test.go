package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const cardOptions = `[{"label":"Option A","what_happens":"A happens","pros":["fast"],"cons":["risky"],"cost_or_time":"1 h"},{"label":"Option B","what_happens":"B happens","pros":["safe"],"cons":["slow"],"cost_or_time":"2 days"}]`

// cardAskJSON is the structured ask a clickable card needs; its recommendation names Option B.
const cardAskJSON = `{"decision":"Pick one","options":` + cardOptions + `,"recommendation":"Option B"}`

func newCardFixture(test *testing.T) askedRecordFixture {
	test.Helper()
	fixture := newLegacyCardFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET ask_json = $2::jsonb WHERE id = $1`, fixture.decision.ID, cardAskJSON)
	return fixture
}

// newLegacyCardFixture is a request stored before ask_json existed: options but no structured ask.
func newLegacyCardFixture(test *testing.T) askedRecordFixture {
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
	if result.Request.Status != "answered" || result.Answer.AnswerSource != "card" || result.Answer.AnsweredByTaskID.Valid || result.Answer.AnsweredByUserID != parseUUID(fixture.humanID) || result.Answer.SourceChatSessionID != parseUUID(fixture.sessionID) || len(result.Answer.ChatMessageIds) != 1 || !result.Answer.WakeAt.Valid || result.Answer.ContentSnapshot != "Option B" || !result.Answer.ChosenOptionIndex.Valid || result.Answer.ChosenOptionIndex.Int32 != 1 {
		test.Fatalf("wrong answered state: %+v %+v", result.Request, result.Answer)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, fixture.issueID); count != 1 {
		test.Fatalf("answers on the issue = %d, want 1", count)
	}
	if result.Comment.DecisionCommentKind.String != "quote" || result.Comment.ParentID != parseUUID(fixture.parentID) || !strings.Contains(result.Comment.Content, "> Chose: Option B") || !strings.Contains(result.Comment.Content, "/chat?session="+fixture.sessionID) {
		test.Fatalf("quote lacks the option or the card link: %+v", result.Comment)
	}
	var role, sentVia string
	var taskless bool
	dbfx.QueryRow(test, `SELECT role, sent_via, task_id IS NULL FROM chat_message WHERE id = ANY($1)`, result.Answer.ChatMessageIds).Scan(&role, &sentVia, &taskless)
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

func TestCardAnswerWithUnsealedIntentWakesCompletedAskingTask(test *testing.T) {
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb, ask_json = $3::jsonb WHERE id = $1`, fixture.decision.ID, cardOptions, cardAskJSON)
	dbfx.Exec(test, `INSERT INTO decision_intent_target (decision_intent_id, human_user_id) VALUES ($1, $2)`, fixture.intentID, testUserID)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = NULL, consumed_at = NULL, consumed_by_request_id = NULL WHERE id = $1`, fixture.intentID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
	if err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NULL`, fixture.intentID) != 1 {
		test.Fatal("fixture intent was sealed before the card answer")
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)

	stored, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: result.Answer.ID})
	if err != nil {
		test.Fatal(err)
	}
	if !stored.WokenAt.Valid || stored.WakeOutcome.String != "run_enqueued" {
		test.Fatalf("card answer wake=%v outcome=%q, want a delivered run", stored.WokenAt.Valid, stored.WakeOutcome.String)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND trigger_comment_id = $2`, fixture.issueID, result.Comment.ID); count != 1 {
		test.Fatalf("card answer runs=%d, want 1", count)
	}
}

func TestProductDirectionCardAnswerWakesCompletedAskingTask(test *testing.T) {
	fixture := newCardFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'product_direction' WHERE id = $1`, fixture.decision.ID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() - interval '1 second' WHERE id = $1`, result.Answer.ID)
	handler, _ := newDecisionRunnerFixture(test)
	if err := deliverDecisionForTest(handler, fixture); err != nil {
		test.Fatalf("deliver product-direction card answer: %v", err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL`, result.Answer.ID); count != 1 {
		test.Fatalf("delivered product-direction card answers=%d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND trigger_comment_id = $2`, fixture.issueID, result.Comment.ID); count != 1 {
		test.Fatalf("product-direction card answer runs=%d, want 1", count)
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
	if err != nil || both.Answer.ContentSnapshot != "Option A\n\nbut wait a day" || !both.Answer.ChosenOptionIndex.Valid || both.Answer.ChosenOptionIndex.Int32 != 0 || both.Answer.ReplyText.String != "but wait a day" {
		test.Fatalf("option plus text = %+v, %v", both.Answer, err)
	}
	if !strings.Contains(both.Comment.Content, "> Chose: Option A\n> \n> Note: but wait a day") {
		test.Fatalf("quote does not label both parts: %q", both.Comment.Content)
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
	input.AskJSON = []byte(`{"decision":"Ship?"}`)
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

func cardFor(test *testing.T, fixture askedRecordFixture) DecisionRequestCard {
	test.Helper()
	cards := testHandler.decisionRequestsForMessages(context.Background(), testWorkspaceID, []db.ChatMessage{{ID: fixture.decision.PingMessageID, ChatSessionID: fixture.decision.ChatSessionID}})
	got := cards[uuidToString(fixture.decision.PingMessageID)]
	if len(got) != 1 {
		test.Fatalf("cards = %+v", got)
	}
	return got[0]
}

func TestCardCarriesProsConsCostAndMarksTheRecommendation(test *testing.T) {
	card := cardFor(test, newCardFixture(test))
	if len(card.Options) != 2 || card.Options[1].Pros[0] != "safe" || card.Options[1].Cons[0] != "slow" || card.Options[1].CostOrTime != "2 days" {
		test.Fatalf("options lack pros, cons or cost: %+v", card.Options)
	}
	if card.RecommendedOptionIndex == nil || *card.RecommendedOptionIndex != 1 {
		test.Fatalf("recommended option = %v, want 1", card.RecommendedOptionIndex)
	}
}

func TestCardWithoutAskJSONIsNotClickable(test *testing.T) {
	fixture := newLegacyCardFixture(test)
	if card := cardFor(test, fixture); len(card.Options) != 0 || card.RecommendedOptionIndex != nil {
		test.Fatalf("a request with ask_json NULL offers buttons: %+v", card)
	}
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err == nil {
		test.Fatal("a click answered a request with ask_json NULL")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID); count != 0 {
		test.Fatalf("refused click left %d answers", count)
	}
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Text: "typed answer"}); err != nil {
		test.Fatalf("a typed answer must still work: %v", err)
	}
}

func TestCardClickOnAHeldClassWaitsTheHold(test *testing.T) {
	fixture := newCardFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, fixture.decision.ID)
	result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"})
	if err != nil {
		test.Fatal(err)
	}
	wait := time.Until(result.Answer.WakeAt.Time)
	if !result.Answer.WakeAt.Valid || wait < 9*time.Minute || wait > 10*time.Minute+5*time.Second {
		test.Fatalf("held click wakes in %v, want about 10 minutes", wait)
	}
	if !result.Answer.ChosenOptionIndex.Valid || result.Answer.ChosenOptionIndex.Int32 != 0 {
		test.Fatalf("chosen index = %+v, want 0", result.Answer.ChosenOptionIndex)
	}
}

// TestCardAnswerOnNeedDecisionIssueWakesTheAssigneeOnce is CHE-4997: a card
// answered while the issue waits in need_decision starts exactly one run for the
// assignee, triggered by the "> Chose: ..." quote, and a second pass adds none.
func TestCardAnswerOnNeedDecisionIssueWakesTheAssigneeOnce(test *testing.T) {
	for _, assignee := range []string{"asker", "other_agent"} {
		test.Run(assignee, func(test *testing.T) {
			resetDecisionLease(test)
			ensureParkCatalog(test)
			fixture := newCardFixture(test)
			assigneeID := fixture.agentID
			if assignee == "other_agent" {
				assigneeID = createHandlerTestAgent(test, "Need-decision owner", []byte("[]"))
			}
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
			dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, fixture.issueID, assigneeID)
			result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
			if err != nil {
				test.Fatal(err)
			}
			_, runner := newDecisionRunnerFixture(test)
			for range 2 {
				decisionPass(test, runner)
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running') AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fixture.issueID, assigneeID, result.Comment.ID); count != 1 {
					test.Fatalf("assignee wake runs = %d, want 1", count)
				}
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'running')`, fixture.issueID); count != 1 {
				test.Fatalf("active runs on the issue = %d, want exactly 1", count)
			}
		})
	}
}

// Negative control: a person who cannot invoke the need_decision assignee never
// gets that agent run by a card answer; the wake stays with the asking agent.
func TestCardAnswerOnNeedDecisionIssueSkipsAnAssigneeThePersonCannotInvoke(test *testing.T) {
	resetDecisionLease(test)
	ensureParkCatalog(test)
	fixture := newCardFixture(test)
	assigneeID := createHandlerTestAgent(test, "Restricted owner", []byte("[]"))
	dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private', owner_id = NULL WHERE id = $1`, assigneeID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, fixture.issueID, assigneeID)
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"}); err != nil {
		test.Fatal(err)
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, fixture.issueID, assigneeID); count != 0 {
		test.Fatalf("restricted assignee runs = %d, want 0", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running')`, fixture.issueID, fixture.agentID); count != 1 {
		test.Fatalf("asking agent runs = %d, want 1", count)
	}
}

func TestCardAnswerOnNeedDecisionIssueRunsNobodyWhenTheAssigneeIsDeniedAndTheAskerIsGone(test *testing.T) {
	resetDecisionLease(test)
	ensureParkCatalog(test)
	fixture := newCardFixture(test)
	assigneeID := createHandlerTestAgent(test, "Restricted owner", []byte("[]"))
	dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private', owner_id = $2 WHERE id = $1`, assigneeID, createEphemeralUser(test, "restricted"))
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, fixture.issueID, assigneeID)
	dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, fixture.agentID)
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"}); err != nil {
		test.Fatal(err)
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, fixture.issueID, assigneeID); count != 0 {
		test.Fatalf("restricted assignee runs = %d, want 0", count)
	}
}

// A card answer must show on the read an agent makes, or the agent keeps
// treating the request as open (CHE-5229).
func TestDecisionGetCarriesTheAnswerCheeseGave(test *testing.T) {
	fixture := newCardFixture(test)
	id := uuidToString(fixture.decision.ID)
	var open map[string]any
	testutil.Call(test, testHandler.GetDecision, withURLParam(newRequest(http.MethodGet, "/api/decisions/"+id, nil), "id", id)).Want(http.StatusOK).JSON(&open)
	if _, has := open["answer"]; has {
		test.Fatalf("open request carries an answer: %+v", open)
	}
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Text: "can you double check"}); err != nil {
		test.Fatal(err)
	}
	var fetched struct {
		Status string `json:"status"`
		Answer *struct {
			AnsweredByUserID string  `json:"answered_by_user_id"`
			AnswerSource     string  `json:"answer_source"`
			Text             string  `json:"content_snapshot"`
			ChosenOption     *int    `json:"chosen_option_index"`
			CreatedAt        string  `json:"created_at"`
			DisclaimedAt     *string `json:"disclaimed_at"`
		} `json:"answer"`
	}
	testutil.Call(test, testHandler.GetDecision, withURLParam(newRequest(http.MethodGet, "/api/decisions/"+id, nil), "id", id)).Want(http.StatusOK).JSON(&fetched)
	if fetched.Status != "answered" || fetched.Answer == nil {
		test.Fatalf("answered request has no answer on read: %+v", fetched)
	}
	if fetched.Answer.AnsweredByUserID != fixture.humanID || fetched.Answer.AnswerSource != "card" || fetched.Answer.Text != "can you double check" || fetched.Answer.ChosenOption != nil || fetched.Answer.CreatedAt == "" || fetched.Answer.DisclaimedAt != nil {
		test.Fatalf("wrong answer on read: %+v", fetched.Answer)
	}
}
