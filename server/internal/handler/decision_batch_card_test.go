package handler

import (
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
