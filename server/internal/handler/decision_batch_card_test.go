package handler

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// batchFixture is a batch card plus the per-issue cards it covers, all
// addressed to the batch card's human (CHE-4453).
type batchFixture struct {
	batch   askedRecordFixture
	covered []askedRecordFixture
}

func newBatchFixture(test *testing.T, covered int) batchFixture {
	test.Helper()
	fixture := batchFixture{batch: newCardFixture(test)}
	ids := make([]string, 0, covered)
	for range covered {
		card := newCardFixture(test)
		dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, card.decision.ID, fixture.batch.humanID)
		dbfx.Exec(test, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, card.sessionID, fixture.batch.humanID)
		ids = append(ids, uuidToString(card.decision.ID))
		fixture.covered = append(fixture.covered, card)
	}
	dbfx.Exec(test, `UPDATE decision_request SET covers_request_ids = $2::uuid[] WHERE id = $1`, fixture.batch.decision.ID, ids)
	return fixture
}

func TestBatchAnswerClosesCoveredCards(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 2)
	for _, card := range fixture.covered {
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, card.taskID, card.chatTaskID)
	}
	result, err := fixture.answerAcknowledged(test, "Option B")
	if err != nil {
		test.Fatal(err)
	}
	for _, card := range fixture.covered {
		var status string
		var by *string
		dbfx.QueryRow(test, `SELECT status, answered_by_batch_request_id::text FROM decision_request WHERE id = $1`, card.decision.ID).Scan(&status, &by)
		if status != "answered" || by == nil || *by != uuidToString(fixture.batch.decision.ID) {
			test.Fatalf("covered card %s: status=%s answered_by_batch=%v, want answered by %s", uuidToString(card.decision.ID), status, by, uuidToString(fixture.batch.decision.ID))
		}
		var quote string
		dbfx.QueryRow(test, `SELECT comment.content FROM comment JOIN decision_answer answer ON answer.id = comment.decision_answer_id WHERE answer.decision_request_id = $1 AND comment.issue_id = $2 AND comment.decision_comment_kind = 'quote'`, card.decision.ID, card.issueID).Scan(&quote)
		if !strings.Contains(quote, "> Chose: Option B") || !strings.Contains(quote, "batch") {
			test.Fatalf("covered issue lacks the batch quote: %q", quote)
		}
		if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND disclaimed_at IS NULL`, card.decision.ID); count != 1 {
			test.Fatalf("live answers on covered card = %d, want 1", count)
		}
	}
	if result.Request.Status != "answered" {
		test.Fatalf("batch card status = %s", result.Request.Status)
	}

	_, runner := newDecisionRunnerFixture(test)
	for range 2 {
		decisionPass(test, runner)
	}
	for _, card := range fixture.covered {
		if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running')`, card.issueID, card.agentID); count != 1 {
			test.Fatalf("covered asking agent wake runs = %d, want exactly 1", count)
		}
	}
}

// answerAcknowledged is a batch option answer from a client that disclosed the
// covered decisions: it carries the digest the server expects.
func (fixture batchFixture) answerAcknowledged(test *testing.T, option string) (decisionRecordResult, error) {
	test.Helper()
	batch, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.batch.issue.WorkspaceID, ID: fixture.batch.decision.ID})
	if err != nil {
		test.Fatal(err)
	}
	return fixture.batch.answerOnCard(decisionCardAnswerInput{Option: option, CoversAck: batchCoverAck(batch, option)})
}

// wantNothingPropagated checks the batch card and every covered card are still open and unanswered.
func (fixture batchFixture) wantNothingPropagated(test *testing.T) {
	test.Helper()
	if status, answers := coveredCardState(test, fixture.batch); status != "open" || answers != 0 {
		test.Fatalf("batch card: status=%s answers=%d, want open with none", status, answers)
	}
	for _, card := range fixture.covered {
		if status, answers := coveredCardState(test, card); status != "open" || answers != 0 {
			test.Fatalf("covered card %s: status=%s answers=%d, want open with none", uuidToString(card.decision.ID), status, answers)
		}
	}
}

func TestBatchAnswerNeedsTheCoverAcknowledgment(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 2)
	batch, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.batch.issue.WorkspaceID, ID: fixture.batch.decision.ID})
	if err != nil {
		test.Fatal(err)
	}
	cases := map[string]decisionCardAnswerInput{
		"old client payload, no acknowledgment": {Option: "Option B"},
		"acknowledgment for another option":     {Option: "Option B", CoversAck: batchCoverAck(batch, "Option A")},
		"acknowledgment that is not a digest":   {Option: "Option B", CoversAck: "yes"},
	}
	for name, input := range cases {
		test.Run(name, func(test *testing.T) {
			_, err := fixture.batch.answerOnCard(input)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest {
				test.Fatalf("err = %v, want a 400 refusal", err)
			}
			fixture.wantNothingPropagated(test)
		})
	}
	// Positive control: the acknowledged payload closes every covered card.
	if _, err := fixture.answerAcknowledged(test, "Option B"); err != nil {
		test.Fatal(err)
	}
	for _, card := range fixture.covered {
		if status, answers := coveredCardState(test, card); status != "answered" || answers != 1 {
			test.Fatalf("covered card after the acknowledged answer: status=%s answers=%d, want answered with one", status, answers)
		}
	}
}

// An acknowledgment is bound to the covered ids: one taken from a different
// batch card does not open this card's answer.
func TestBatchAcknowledgmentIsBoundToTheCoveredCards(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 2)
	other := newBatchFixture(test, 1)
	stolen, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: other.batch.issue.WorkspaceID, ID: other.batch.decision.ID})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.batch.answerOnCard(decisionCardAnswerInput{Option: "Option B", CoversAck: batchCoverAck(stolen, "Option B")}); err == nil {
		test.Fatal("an acknowledgment of another batch card was accepted")
	}
	fixture.wantNothingPropagated(test)
}

// Pins the digest so the web client can compute the same value.
func TestBatchCoverAckDigestIsStable(test *testing.T) {
	batch := db.DecisionRequest{ID: parseUUID("00000000-0000-4000-8000-0000000000b1"), CoversRequestIds: []pgtype.UUID{parseUUID("00000000-0000-4000-8000-0000000000c2"), parseUUID("00000000-0000-4000-8000-0000000000c1")}}
	const want = "012ee7331cfe77d20ca96126e0cae16df3acabee3149e329bb3054f42d0211e1"
	if got := batchCoverAck(batch, "Option B"); got != want {
		test.Fatalf("digest = %s, want %s", got, want)
	}
}

func batchCreateRequest(fixture requestCoreFixture, covers []string) *http.Request {
	body := map[string]any{
		"issue_id": fixture.issueID, "session_id": fixture.sessionID,
		"ask": fixture.structuredAsk(), "escalation_class": "reversible", "covers_request_ids": covers,
	}
	request := newRequest(http.MethodPost, "/api/decisions", body)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return request
}

func TestBatchCardCreationStoresCovers(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	card := newCardFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, card.decision.ID, fixture.humanID)
	shareHoldProceed(test, card)
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, batchCreateRequest(fixture, []string{uuidToString(card.decision.ID), uuidToString(card.decision.ID)})).Want(http.StatusCreated).JSON(&created)
	var covers []string
	dbfx.QueryRow(test, `SELECT covers_request_ids::text[] FROM decision_request WHERE id = $1`, created["id"]).Scan(&covers)
	if len(covers) != 1 || covers[0] != uuidToString(card.decision.ID) {
		test.Fatalf("covers = %v, want the one covered card once", covers)
	}
}

// notPlainChoiceCards is one row per kind of card in the decision-card model
// that a batch card must not cover: only a plain choice card is on the
// allowlist, so a new kind of card is closed to batches until it is added there.
// The plain choice card itself is the positive control in each test that uses it.
func notPlainChoiceCards() map[string]func(test *testing.T, card askedRecordFixture) {
	setClass := func(class string) func(test *testing.T, card askedRecordFixture) {
		return func(test *testing.T, card askedRecordFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = $2 WHERE id = $1`, card.decision.ID, class)
		}
	}
	return map[string]func(test *testing.T, card askedRecordFixture){
		"spend gate":   setClass("spend"),
		"deploy gate":  setClass("deploy"),
		"publish gate": setClass("publish"),
		"irreversible": setClass("irreversible"),
		"human action": func(test *testing.T, card askedRecordFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET kind = 'action' WHERE id = $1`, card.decision.ID)
		},
		"unprompted": func(test *testing.T, card askedRecordFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET origin = 'unprompted' WHERE id = $1`, card.decision.ID)
		},
		"agents authority": func(test *testing.T, card askedRecordFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET authority = 'agents' WHERE id = $1`, card.decision.ID)
		},
		"governed write": func(test *testing.T, card askedRecordFixture) { makeBoundWriteCard(test, card) },
		"artifact approval": func(test *testing.T, card askedRecordFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET ask_json = ask_json || '{"subject":"artifact_approval","artifacts":[{"kind":"file","name":"plan.md"}]}'::jsonb WHERE id = $1`, card.decision.ID)
		},
		"attached action": func(test *testing.T, card askedRecordFixture) {
			dbfx.Exec(test, `INSERT INTO decision_action (workspace_id, decision_request_id, action_type, action_version, params, payload_sha256, expires_at, requested_by_agent_id)
				SELECT workspace_id, id, 'agent.env.patch', 1, '{}', repeat('a', 64), now() + interval '1 day', $2 FROM decision_request WHERE id = $1`, card.decision.ID, card.agentID)
		},
		"governed write record": func(test *testing.T, card askedRecordFixture) {
			dbfx.Exec(test, `INSERT INTO decision_bound_write (decision_id, workspace_id, agent_id, field, before_sha256, after_sha256)
				SELECT id, workspace_id, asking_agent_id, 'env', repeat('a', 64), repeat('b', 64) FROM decision_request WHERE id = $1`, card.decision.ID)
		},
	}
}

// A card that is not a plain choice card is refused at creation, whatever its
// kind (CHE-4453 round 7).
func TestBatchCardRefusesEveryNotPlainChoiceCard(test *testing.T) {
	for name, mutate := range notPlainChoiceCards() {
		test.Run(name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			card := newCardFixture(test)
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, card.decision.ID, fixture.humanID)
			shareHoldProceed(test, card)
			mutate(test, card)
			testutil.Call(test, testHandler.RequestDecision, batchCreateRequest(fixture, []string{uuidToString(card.decision.ID)})).Want(http.StatusBadRequest)
			fixture.assertCounts(test, 0)
		})
	}
}

// A card that turns into a not plain choice card after the batch card was
// created stays open when the batch is answered; the plain card beside it closes.
func TestBatchAnswerLeavesEveryNotPlainChoiceCardOpen(test *testing.T) {
	for name, mutate := range notPlainChoiceCards() {
		test.Run(name, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newBatchFixture(test, 2)
			mutate(test, fixture.covered[0])
			if _, err := fixture.answerAcknowledged(test, "Option B"); err != nil {
				test.Fatal(err)
			}
			if status, answers := coveredCardState(test, fixture.covered[0]); status != "open" || answers != 0 {
				test.Fatalf("not plain card: status=%s answers=%d, want open with none", status, answers)
			}
			if status, answers := coveredCardState(test, fixture.covered[1]); status != "answered" || answers != 1 {
				test.Fatalf("plain card: status=%s answers=%d, want answered with one", status, answers)
			}
		})
	}
}

func TestBatchCardCreationRefusals(test *testing.T) {
	cases := map[string]func(card askedRecordFixture, fixture requestCoreFixture){
		"another human's card": func(card askedRecordFixture, fixture requestCoreFixture) {},
		"gated class": func(card askedRecordFixture, fixture requestCoreFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2, escalation_class = 'deploy' WHERE id = $1`, card.decision.ID, fixture.humanID)
		},
		"governed write approval": func(card askedRecordFixture, fixture requestCoreFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, card.decision.ID, fixture.humanID)
			makeBoundWriteCard(test, card)
		},
		"unrelated decision": func(card askedRecordFixture, fixture requestCoreFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, card.decision.ID, fixture.humanID)
			setCardOptions(test, card, unrelatedOptions)
		},
		"same label with a different meaning": func(card askedRecordFixture, fixture requestCoreFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, card.decision.ID, fixture.humanID)
			setCardOptions(test, card, sameLabelsOtherMeaning)
		},
		"artifact approval": func(card askedRecordFixture, fixture requestCoreFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2, ask_json = ask_json || '{"subject":"artifact_approval","artifacts":[{"kind":"file","name":"plan.md"}]}'::jsonb WHERE id = $1`, card.decision.ID, fixture.humanID)
		},
		"already answered": func(card askedRecordFixture, fixture requestCoreFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2, status = 'answered' WHERE id = $1`, card.decision.ID, fixture.humanID)
		},
	}
	for name, setup := range cases {
		test.Run(name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			card := newCardFixture(test)
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = '00000000-0000-0000-0000-000000000001' WHERE id = $1`, card.decision.ID)
			shareHoldProceed(test, card)
			setup(card, fixture)
			testutil.Call(test, testHandler.RequestDecision, batchCreateRequest(fixture, []string{uuidToString(card.decision.ID)})).Want(http.StatusBadRequest)
			fixture.assertCounts(test, 0)
		})
	}
}

// makeBoundWriteCard turns an open card into a decision-bound governed-write
// approval card, as newBoundWriteFixture does before it answers the card.
func makeBoundWriteCard(test *testing.T, card askedRecordFixture) {
	test.Helper()
	options := `[{"label":"` + decisiontx.ApproveWriteLabel + `","what_happens":"apply"},{"label":"Reject","what_happens":"drop"}]`
	dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb WHERE id = $1`, card.decision.ID, options)
	storeBoundWriteCard(test, card.decision.ID, decisiontx.BoundWriteDecision(boundField, boundBefore, boundAfter))
}

// A batch answer must not approve a governed write the human never saw: the
// covered write card stays open and unanswered, so the binding is refused.
func TestBatchAnswerLeavesGovernedWriteCardOpen(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 1)
	write := fixture.covered[0]
	makeBoundWriteCard(test, write)
	dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, write.issue.WorkspaceID, parseUUID(fixture.batch.humanID))
	dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb WHERE id = $1`, fixture.batch.decision.ID,
		`[{"label":"`+decisiontx.ApproveWriteLabel+`","what_happens":"ok"},{"label":"Reject","what_happens":"no"}]`)
	if _, err := fixture.answerAcknowledged(test, decisiontx.ApproveWriteLabel); err != nil {
		test.Fatal(err)
	}
	var status string
	dbfx.QueryRow(test, `SELECT status FROM decision_request WHERE id = $1`, write.decision.ID).Scan(&status)
	if status != "open" || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, write.decision.ID) != 0 {
		test.Fatalf("governed write card was closed by the batch answer: status=%s", status)
	}
	if err := write.verifyBinding(allowAnyAgent, boundAfter); err == nil || !strings.Contains(err.Error(), "no live answer") {
		test.Fatalf("binding on a batch-covered write card = %v, want refusal: decision has no live answer", err)
	}
}

// Positive control: answering the governed-write card itself still binds.
func TestDirectGovernedWriteAnswerStillBinds(test *testing.T) {
	resetDecisionLease(test)
	write := newBoundWriteFixture(test, decisiontx.ApproveWriteLabel)
	if err := write.verifyBinding(allowAnyAgent, boundAfter); err != nil {
		test.Fatalf("direct approval no longer binds: %v", err)
	}
}

// sameLabelsOtherMeaning offers the API test batch card's labels with other actions behind them.
const sameLabelsOtherMeaning = `[{"label":"Hold","what_happens":"Delete the production data."},{"label":"Proceed","what_happens":"Proceed happens."}]`

// differentMeaningOptions keeps the fixture batch card's labels and changes what one of them does.
const differentMeaningOptions = `[{"label":"Option A","what_happens":"A happens"},{"label":"Option B","what_happens":"Wipe the archive."}]`

const unrelatedOptions = `[{"label":"Ship the redesign","what_happens":"ships"},{"label":"Drop the redesign","what_happens":"drops"}]`

func setCardOptions(test *testing.T, card askedRecordFixture, options string) {
	test.Helper()
	dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb WHERE id = $1`, card.decision.ID, options)
}

// shareHoldProceed gives a covered card the two options the API test's batch ask offers.
func shareHoldProceed(test *testing.T, card askedRecordFixture) {
	test.Helper()
	setCardOptions(test, card, `[{"label":"Hold","what_happens":"Hold happens."},{"label":"Proceed","what_happens":"Proceed happens."}]`)
}

func coveredCardState(test *testing.T, card askedRecordFixture) (string, int) {
	test.Helper()
	var status string
	dbfx.QueryRow(test, `SELECT status FROM decision_request WHERE id = $1`, card.decision.ID).Scan(&status)
	return status, dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, card.decision.ID)
}

// A batch answer closes only a card that offers the answer chosen on the
// batch card. A card with unrelated options stays open for its own answer
// (CHE-4453 review P1). The related card beside it is the positive control.
func TestBatchAnswerLeavesUnrelatedCardOpen(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 2)
	unrelated, related := fixture.covered[0], fixture.covered[1]
	setCardOptions(test, unrelated, unrelatedOptions)
	if _, err := fixture.answerAcknowledged(test, "Option B"); err != nil {
		test.Fatal(err)
	}
	if status, answers := coveredCardState(test, unrelated); status != "open" || answers != 0 {
		test.Fatalf("unrelated covered card: status=%s answers=%d, want open with none", status, answers)
	}
	if status, answers := coveredCardState(test, related); status != "answered" || answers != 1 {
		test.Fatalf("related covered card: status=%s answers=%d, want answered with one", status, answers)
	}
}

// A card whose chosen label means something else than on the batch card stays
// open: the label alone is not consent to another action (CHE-4453 review P1).
func TestBatchAnswerLeavesDifferentMeaningCardOpen(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 2)
	different, same := fixture.covered[0], fixture.covered[1]
	setCardOptions(test, different, differentMeaningOptions)
	if _, err := fixture.answerAcknowledged(test, "Option B"); err != nil {
		test.Fatal(err)
	}
	if status, answers := coveredCardState(test, different); status != "open" || answers != 0 {
		test.Fatalf("same-label covered card with another meaning: status=%s answers=%d, want open with none", status, answers)
	}
	if status, answers := coveredCardState(test, same); status != "answered" || answers != 1 {
		test.Fatalf("same-meaning covered card: status=%s answers=%d, want answered with one", status, answers)
	}
}

// A free-text batch answer names no option, so it closes no covered card.
func TestBatchTextAnswerLeavesCoveredCardsOpen(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 1)
	if _, err := fixture.batch.answerOnCard(decisionCardAnswerInput{Text: "Ship it all."}); err != nil {
		test.Fatal(err)
	}
	if status, answers := coveredCardState(test, fixture.covered[0]); status != "open" || answers != 0 {
		test.Fatalf("covered card after a text answer: status=%s answers=%d, want open with none", status, answers)
	}
}

// The batch card lists every covered decision, so the human sees what the answer decides.
func TestBatchCardDisclosesCoveredDecisions(test *testing.T) {
	fixture := newBatchFixture(test, 2)
	request := withURLParam(httptest.NewRequest(http.MethodGet, "/api/chat/sessions/"+fixture.batch.sessionID+"/messages/page", nil), "sessionId", fixture.batch.sessionID)
	request.Header.Set("X-User-ID", fixture.batch.humanID)
	request = withChatTestWorkspaceCtx(test, request)
	var page struct {
		Messages []struct {
			DecisionRequest *DecisionRequestCard `json:"decision_request"`
		} `json:"messages"`
	}
	testutil.Call(test, testHandler.ListChatMessagesPage, request).Want(http.StatusOK).JSON(&page)
	var batch *DecisionRequestCard
	for _, message := range page.Messages {
		if message.DecisionRequest != nil && message.DecisionRequest.ID == uuidToString(fixture.batch.decision.ID) {
			batch = message.DecisionRequest
		}
	}
	if batch == nil || len(batch.CoveredCards) != 2 {
		test.Fatalf("batch card covered_cards = %+v, want 2 entries", batch)
	}
	for index, card := range batch.CoveredCards {
		if card.ID != uuidToString(fixture.covered[index].decision.ID) || card.Decision != "Pick one" || card.IssueIdentifier == "" {
			test.Fatalf("covered_cards[%d] = %+v, want the covered card's id, decision and issue", index, card)
		}
		if len(card.Options) != 2 || card.Options[1].Label != "Option B" || card.Options[1].WhatHappens != "B happens" {
			test.Fatalf("covered_cards[%d].options = %+v, want each covered option with its meaning", index, card.Options)
		}
	}
}

// When the covered decisions cannot be read, the lookup reports the error
// instead of an empty list, so the card cannot look like it covers nothing.
func TestCoveredCardSummariesReportReadFailure(test *testing.T) {
	fixture := newBatchFixture(test, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summaries, err := testHandler.coveredCardSummaries(ctx, parseUUID(testWorkspaceID), []pgtype.UUID{fixture.covered[0].decision.ID})
	if err == nil || len(summaries) != 0 {
		test.Fatalf("summaries=%v err=%v, want an error and no summaries", summaries, err)
	}
}

// The issue page lists open cards from /api/decisions/open. A batch card there
// must carry the covered decisions too, or the page would let a human answer
// for cards it never shows.
func TestOpenDecisionsListDisclosesCoveredDecisions(test *testing.T) {
	fixture := newBatchFixture(test, 2)
	dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1 OR id = ANY($3::uuid[])`,
		fixture.batch.decision.ID, parseUUID(testUserID), []pgtype.UUID{fixture.covered[0].decision.ID, fixture.covered[1].decision.ID})
	request := httptest.NewRequest(http.MethodGet, "/api/decisions/open", nil)
	request.Header.Set("X-User-ID", testUserID)
	request = withChatTestWorkspaceCtx(test, request)
	var open []struct {
		ID               string                       `json:"id"`
		CoversRequestIDs []string                     `json:"covers_request_ids"`
		CoveredCards     []DecisionRequestCoveredCard `json:"covered_cards"`
	}
	testutil.Call(test, testHandler.ListOpenDecisions, request).Want(http.StatusOK).JSON(&open)
	var batch *int
	for index := range open {
		if open[index].ID == uuidToString(fixture.batch.decision.ID) {
			batch = &index
		}
	}
	if batch == nil {
		test.Fatalf("open list lacks the batch card: %+v", open)
	}
	got := open[*batch]
	if len(got.CoversRequestIDs) != 2 || len(got.CoveredCards) != 2 {
		test.Fatalf("batch card covers=%v covered_cards=%+v, want 2 and 2", got.CoversRequestIDs, got.CoveredCards)
	}
	for index, card := range got.CoveredCards {
		if card.ID != uuidToString(fixture.covered[index].decision.ID) || card.Decision != "Pick one" || len(card.Options) != 2 || card.Options[1].WhatHappens != "B happens" {
			test.Fatalf("covered_cards[%d] = %+v, want the covered card's decision and options with meanings", index, card)
		}
	}
}

// Withdrawing the batch answer withdraws what it decided on the covered cards:
// they reopen, their answers are disclaimed, and no live answer stays behind.
func TestDisclaimingABatchAnswerWithdrawsTheCoveredAnswers(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 2)
	result, err := fixture.answerAcknowledged(test, "Option B")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := testHandler.disclaimDecisionAnswerTx(fixture.batch.request(), fixture.batch.issue.WorkspaceID, result.Answer.ID, parseUUID(fixture.batch.humanID)); err != nil {
		test.Fatal(err)
	}
	if status, _ := coveredCardState(test, fixture.batch); status != "open" {
		test.Fatalf("batch card status = %s, want open", status)
	}
	for _, card := range fixture.covered {
		var status string
		var by *string
		dbfx.QueryRow(test, `SELECT status, answered_by_batch_request_id::text FROM decision_request WHERE id = $1`, card.decision.ID).Scan(&status, &by)
		if status != "open" || by != nil {
			test.Fatalf("covered card %s after the batch disclaim: status=%s answered_by_batch=%v, want open and unfolded", uuidToString(card.decision.ID), status, by)
		}
		if live := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND disclaimed_at IS NULL`, card.decision.ID); live != 0 {
			test.Fatalf("covered card %s keeps %d live answers after the batch disclaim", uuidToString(card.decision.ID), live)
		}
	}
}

// A covered product-direction card keeps the recovery restriction its own
// answer has: a batch answer cannot settle it while the person is in recovery.
func TestBatchAnswerCannotSettleAGatedCoveredCardWhileRecovering(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 1)
	workspaceID := uuidToString(fixture.batch.issue.WorkspaceID)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'product_direction' WHERE id = $1`, fixture.covered[0].decision.ID)
	dbfx.Exec(test, `UPDATE workspace SET decision_passkey_gate = 'block' WHERE id = $1`, workspaceID)
	dbfx.Cleanup(test, `UPDATE workspace SET decision_passkey_gate = 'off' WHERE id = $1`, workspaceID)
	setPasskeyState(test, fixture.batch.humanID, "recovery_required")
	_, err := fixture.answerAcknowledged(test, "Option B")
	if err == nil || !strings.Contains(err.Error(), "recovery_required") {
		test.Fatalf("err = %v, want recovery_required", err)
	}
	fixture.wantNothingPropagated(test)
}

// Every path that withdraws or ends a batch answer must take the covered answers
// with it, so no covered card keeps a live answer that rests on a gone batch
// answer (CHE-4453 round 6). One case per production caller of the withdrawal.
func TestEveryBatchAnswerWithdrawalReopensTheCoveredCards(test *testing.T) {
	paths := map[string]func(test *testing.T, fixture batchFixture, result decisionRecordResult){
		"human disclaim": func(test *testing.T, fixture batchFixture, result decisionRecordResult) {
			if _, err := testHandler.disclaimDecisionAnswerTx(fixture.batch.request(), fixture.batch.issue.WorkspaceID, result.Answer.ID, parseUUID(fixture.batch.humanID)); err != nil {
				test.Fatal(err)
			}
		},
		"agent veto": func(test *testing.T, fixture batchFixture, result decisionRecordResult) {
			dbfx.Exec(test, `UPDATE decision_answer SET wake_at = now(), woken_at = now(), wake_outcome = 'run_enqueued' WHERE id = $1`, result.Answer.ID)
			if response := vetoDecisionHTTP(testHandler, fixture.batch, uuidToString(result.Comment.ID), "Wrong batch answer", true); response.Code != http.StatusCreated {
				test.Fatalf("veto status=%d body=%s", response.Code, response.Body.String())
			}
		},
		"answer source removed": func(test *testing.T, fixture batchFixture, result decisionRecordResult) {
			dbfx.Exec(test, `DELETE FROM chat_message WHERE id = ANY($1)`, result.Answer.ChatMessageIds)
			if _, err := testHandler.CheckDecisionSource(context.Background(), fixture.batch.issue.WorkspaceID, fixture.batch.decision.ID); err != nil {
				test.Fatal(err)
			}
		},
		"lifecycle cancel": func(test *testing.T, fixture batchFixture, result decisionRecordResult) {
			dbfx.Exec(test, `UPDATE issue SET status = 'cancelled' WHERE id = $1`, fixture.batch.issueID)
			if _, err := testHandler.CancelDecisionLifecycle(context.Background(), fixture.batch.issue.WorkspaceID, fixture.batch.decision.ID); err != nil {
				test.Fatal(err)
			}
		},
		"source issue deleted": func(test *testing.T, fixture batchFixture, result decisionRecordResult) {
			dbfx.Exec(test, `DELETE FROM issue WHERE id = $1`, fixture.batch.issueID)
			if err := testHandler.cleanupDecisionOrphans(context.Background(), fixture.batch.issue.WorkspaceID, fixture.batch.issue.ID); err != nil {
				test.Fatal(err)
			}
		},
		"class raised past a delegate": func(test *testing.T, fixture batchFixture, result decisionRecordResult) {
			placeDecisionHuman(test, fixture.batch.issueID, fixture.batch.humanID, "member", true)
			if response := raiseDecisionClassAs(test, fixture.batch, fixture.batch.decision.ID, "spend"); response.Code != http.StatusOK {
				test.Fatalf("raise status=%d body=%s", response.Code, response.Body.String())
			}
		},
	}
	// A covered card's DM can be gone by the time its batch answer is withdrawn
	// (CHE-4453 round 8): the withdrawal must still finish, cancelling the card
	// whose lane no longer exists instead of reopening it.
	for _, dmDeleted := range []bool{false, true} {
		for name, withdraw := range paths {
			if dmDeleted {
				name += " with a covered DM deleted"
			}
			test.Run(name, func(test *testing.T) {
				resetDecisionLease(test)
				fixture := newBatchFixture(test, 2)
				for _, card := range fixture.covered {
					dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, card.taskID, card.chatTaskID)
				}
				result, err := fixture.answerAcknowledged(test, "Option B")
				if err != nil {
					test.Fatal(err)
				}
				if dmDeleted {
					dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, fixture.covered[0].sessionID)
					dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, fixture.covered[0].sessionID)
				}
				withdraw(test, fixture, result)
				if !strings.HasPrefix(name, "lifecycle cancel") && !strings.HasPrefix(name, "source issue deleted") && dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND disclaimed_at IS NULL`, fixture.batch.decision.ID) > 0 {
					test.Fatal("batch answer is still live after the withdrawal")
				}
				for index, card := range fixture.covered {
					var status string
					var by *string
					dbfx.QueryRow(test, `SELECT status, answered_by_batch_request_id::text FROM decision_request WHERE id = $1`, card.decision.ID).Scan(&status, &by)
					if dmDeleted && index == 0 {
						if status != "cancelled" {
							test.Fatalf("covered card %s with its DM deleted: status=%s, want cancelled", uuidToString(card.decision.ID), status)
						}
					} else if status != "open" || by != nil {
						test.Fatalf("covered card %s: status=%s answered_by_batch=%v, want open and unfolded", uuidToString(card.decision.ID), status, by)
					}
					if live := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND disclaimed_at IS NULL`, card.decision.ID); live != 0 {
						test.Fatalf("covered card %s keeps %d live answers", uuidToString(card.decision.ID), live)
					}
				}
				// Later delivery must not resurrect a withdrawn answer.
				_, runner := newDecisionRunnerFixture(test)
				for range 2 {
					decisionPass(test, runner)
				}
				for index, card := range fixture.covered {
					// The one run a card with a deleted DM may have is the re-ask notice its
					// asking agent is owed; a withdrawn answer wakes nothing, and more passes add nothing.
					want := 0
					if dmDeleted && index == 0 {
						want = 1
					}
					for range 2 {
						decisionPass(test, runner)
					}
					if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running')`, card.issueID, card.agentID); count != want {
						test.Fatalf("covered card %s has %d queued runs after the withdrawal, want %d", uuidToString(card.decision.ID), count, want)
					}
					if live := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND disclaimed_at IS NULL`, card.decision.ID); live != 0 {
						test.Fatalf("covered card %s has %d live answers after delivery", uuidToString(card.decision.ID), live)
					}
				}
			})
		}
	}
}

// A covered card's answer keeps its DM: the session retention query names a
// card a batch answer closed, so deleting the DM is refused like any other
// referenced DM (CHE-4453 round 8).
func TestDeletingTheDMOfACardABatchAnswerClosedIsRefused(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBatchFixture(test, 2)
	if _, err := fixture.answerAcknowledged(test, "Option B"); err != nil {
		test.Fatal(err)
	}
	for _, card := range fixture.covered {
		references, err := testHandler.Queries.ListDecisionSessionReferences(context.Background(), db.ListDecisionSessionReferencesParams{WorkspaceID: card.issue.WorkspaceID, SessionID: parseUUID(card.sessionID)})
		if err != nil {
			test.Fatal(err)
		}
		if len(references) == 0 {
			test.Fatalf("DM %s of covered card %s is not retained after the batch answer", card.sessionID, uuidToString(card.decision.ID))
		}
	}
}
