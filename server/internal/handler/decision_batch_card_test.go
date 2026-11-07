package handler

import (
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	"net/http"
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
	result, err := fixture.batch.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
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
	var created map[string]any
	testutil.Call(test, testHandler.RequestDecision, batchCreateRequest(fixture, []string{uuidToString(card.decision.ID), uuidToString(card.decision.ID)})).Want(http.StatusCreated).JSON(&created)
	var covers []string
	dbfx.QueryRow(test, `SELECT covers_request_ids::text[] FROM decision_request WHERE id = $1`, created["id"]).Scan(&covers)
	if len(covers) != 1 || covers[0] != uuidToString(card.decision.ID) {
		test.Fatalf("covers = %v, want the one covered card once", covers)
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
		"already answered": func(card askedRecordFixture, fixture requestCoreFixture) {
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2, status = 'answered' WHERE id = $1`, card.decision.ID, fixture.humanID)
		},
	}
	for name, setup := range cases {
		test.Run(name, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			card := newCardFixture(test)
			dbfx.Exec(test, `UPDATE decision_request SET human_user_id = '00000000-0000-0000-0000-000000000001' WHERE id = $1`, card.decision.ID)
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
	if _, err := fixture.batch.answerOnCard(decisionCardAnswerInput{Option: decisiontx.ApproveWriteLabel}); err != nil {
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
