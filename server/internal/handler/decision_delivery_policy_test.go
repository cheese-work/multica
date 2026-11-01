package handler

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// taskDigest fingerprints every task, wakeup receipt and scoped wakeup event of
// the issue, so a refusal that captures anything changes it.
func taskDigest(test *testing.T, issueID string) string {
	test.Helper()
	var digest string
	dbfx.QueryRow(test, `SELECT md5(COALESCE((SELECT string_agg(t::text, '|' ORDER BY t.id) FROM agent_task_queue t WHERE t.issue_id = $1), '')
		|| COALESCE((SELECT string_agg(r::text, '|' ORDER BY r.id) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id = r.wakeup_id WHERE w.issue_id = $1), '')
		|| COALESCE((SELECT string_agg(e::text, '|' ORDER BY e.id) FROM wakeup_scoped_event e WHERE e.issue_id = $1), ''))`, issueID).Scan(&digest)
	return digest
}

func TestDecisionDeliveryRefusesAgentRecordOfGatedClass(test *testing.T) {
	for _, branch := range []string{"enqueue", "queued", "running", "wakeup", "group"} {
		test.Run(branch, func(test *testing.T) {
			resetDecisionLease(test)
			handler, _ := newDecisionRunnerFixture(test)
			var issueID string
			// Leftover runs would be claimed by later tests on the shared runtime.
			defer func() {
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, issueID)
			}()
			if branch == "group" {
				group := readyDecisionDeliveryGroup(test)
				issueID = group.core.issueID
				forbidden, permitted := group.records[0], group.records[1]
				dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, forbidden.Request.ID)
				dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = ANY($1)`, []string{uuidToString(forbidden.Answer.ID), uuidToString(permitted.Answer.ID)})
				before := taskDigest(test, issueID)
				if err := deliverDecisionForTest(handler, group.members[0]); err != nil {
					test.Fatal(err)
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NULL AND disclaim_reason IS NULL`, permitted.Answer.ID); count != 1 {
					test.Fatal("permitted group member was woken or withdrawn")
				}
				assertNoDeliveryEffect(test, issueID, before, forbidden, permitted)
				assertWithdrawnForCard(test, forbidden)
				return
			}
			fixture := newAskedRecordFixture(test)
			recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, recorded.Request.ID)
			dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
			dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
			switch branch {
			case "queued", "running":
				active := dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": branch, "runtime_id": handlerTestRuntimeID(test)})
				dbfx.Exec(test, `UPDATE agent_task_queue SET comment_thread_id = $1 WHERE id = $2`, fixture.tagID, active)
			case "wakeup":
				dbfx.Insert(test, "issue_wakeup", testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "agent_id": fixture.agentID,
					"created_by": testUserID, "instruction": "resume", "kind": "event", "mode": "continuous", "event_types": testutil.Raw("ARRAY['comment.created']")})
			}
			issueID = fixture.issueID
			before := taskDigest(test, issueID)
			if err := deliverDecisionForTest(handler, fixture); err != nil {
				test.Fatal(err)
			}
			assertNoDeliveryEffect(test, issueID, before, recorded)
			assertWithdrawnForCard(test, recorded)
		})
	}
}

func assertNoDeliveryEffect(test *testing.T, issueID, before string, records ...decisionRecordResult) {
	test.Helper()
	if after := taskDigest(test, issueID); after != before {
		test.Fatal("delivery captured a wakeup or created, merged into or changed a task")
	}
	for _, record := range records {
		if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND (woken_at IS NOT NULL OR wake_outcome IS NOT NULL)`, record.Answer.ID); count != 0 {
			test.Fatal("answer was acknowledged")
		}
		if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'delivery'`, record.Answer.ID); count != 0 {
			test.Fatal("delivery wrote an effect comment")
		}
	}
}

func assertWithdrawnForCard(test *testing.T, record decisionRecordResult) {
	test.Helper()
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NOT NULL AND disclaim_reason = 'card_required' AND disclaimed_by_user_id IS NULL AND disclaimed_by_agent_id IS NULL`, record.Answer.ID); count != 1 {
		test.Fatal("forbidden answer was not withdrawn with card_required")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, record.Request.ID); count != 1 {
		test.Fatal("forbidden answer's request was not reopened")
	}
	var content string
	dbfx.QueryRow(test, `SELECT content FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'disclaim'`, record.Answer.ID).Scan(&content)
	if !strings.Contains(content, "/chat?session=") {
		test.Fatalf("withdrawal comment lacks the card link: %q", content)
	}
}

func TestDecisionDeliveryReplayKeepsWokenAnswer(test *testing.T) {
	resetDecisionLease(test)
	handler, _ := newDecisionRunnerFixture(test)
	fixture, recorded := readyDecisionDelivery(test)
	defer dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, fixture.issueID)
	if err := deliverDecisionForTest(handler, fixture); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL`, recorded.Answer.ID); count != 1 {
		test.Fatal("first delivery did not wake the answer")
	}
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, recorded.Request.ID)
	before := taskDigest(test, fixture.issueID)
	if err := deliverDecisionForTest(handler, fixture); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NULL`, recorded.Answer.ID); count != 1 {
		test.Fatal("replay withdrew an answer that was already delivered")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, recorded.Request.ID); count != 1 {
		test.Fatal("replay reopened a delivered request")
	}
	if after := taskDigest(test, fixture.issueID); after != before {
		test.Fatal("replay changed tasks or wakeup captures")
	}
}

func TestDecisionDeliveryRefusedUnpromptedRecordTakesCardAnswer(test *testing.T) {
	resetDecisionLease(test)
	handler, _ := newDecisionRunnerFixture(test)
	fixture := newCardFixture(test)
	defer dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, fixture.issueID)
	input := fixture.recordInput()
	input.ParentID, input.EscalationClass = parseUUID(dbfx.Comment(test, fixture.issueID, "An ordinary thread")), "spend"
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil || recorded.Request.Origin != "unprompted" || recorded.Answer.AnswerSource != "agent_record" {
		test.Fatalf("not a gated unprompted agent record: %+v %v", recorded, err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
	before := taskDigest(test, fixture.issueID)
	unprompted := fixture
	unprompted.decision = recorded.Request
	if err := deliverDecisionForTest(handler, unprompted); err != nil {
		test.Fatal(err)
	}
	assertNoDeliveryEffect(test, fixture.issueID, before, recorded)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaim_reason = 'card_required'`, recorded.Answer.ID); count != 1 {
		test.Fatal("unprompted agent record was not withdrawn with card_required")
	}
	var content string
	dbfx.QueryRow(test, `SELECT content FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'disclaim'`, recorded.Answer.ID).Scan(&content)
	if !strings.Contains(content, "new decision request") {
		test.Fatalf("withdrawal comment does not tell the agent to ask again: %q", content)
	}
	// The replacement is a fresh asked request, answered on its card.
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, fixture.decision.ID)
	card, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"})
	if err != nil {
		test.Fatal(err)
	}
	// Stamp the answer as the card's own, as a card click records it.
	dbfx.Exec(test, `UPDATE decision_answer SET answer_source = 'card', answered_by_user_id = $2, answered_by_task_id = NULL, source_chat_session_id = NULL, source_comment_id = NULL,
		chat_message_ids = '{}', wake_at = clock_timestamp() WHERE id = $1`, card.Answer.ID, fixture.humanID)
	if err := deliverDecisionForTest(handler, fixture); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running') AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fixture.issueID, fixture.agentID, card.Comment.ID); count != 1 {
		test.Fatalf("card answer runs = %d, want 1", count)
	}
}
