package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
	assertAnswerWithdrawn(test, record, "open")
}

// assertAnswerWithdrawn checks the card_required withdrawal and the state the
// request is left in: open for an asked request, cancelled for an unprompted record.
func assertAnswerWithdrawn(test *testing.T, record decisionRecordResult, requestStatus string) {
	test.Helper()
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NOT NULL AND disclaim_reason = 'card_required' AND disclaimed_by_user_id IS NULL AND disclaimed_by_agent_id IS NULL`, record.Answer.ID); count != 1 {
		test.Fatal("forbidden answer was not withdrawn with card_required")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = $2`, record.Request.ID, requestStatus); count != 1 {
		test.Fatalf("forbidden answer's request is not %s", requestStatus)
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

// A real card answer is a card answer: it is stamped by the production path,
// never rewritten, and it wakes exactly once.
func TestDecisionDeliveryWakesRealCardAnswerOnce(test *testing.T) {
	resetDecisionLease(test)
	handler, _ := newDecisionRunnerFixture(test)
	fixture := newCardFixture(test)
	defer dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, fixture.decision.ID)
	card, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"})
	if err != nil {
		test.Fatal(err)
	}
	if card.Answer.AnswerSource != "card" || card.Answer.AnsweredByUserID != parseUUID(fixture.humanID) || card.Answer.AnsweredByTaskID.Valid {
		test.Fatalf("card answer not stamped as a card: %+v", card.Answer)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, card.Answer.ID)
	for range 2 {
		if err := deliverDecisionForTest(handler, fixture); err != nil {
			test.Fatal(err)
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL AND disclaimed_at IS NULL`, card.Answer.ID); count != 1 {
		test.Fatal("real card answer was refused or not delivered")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'running') AND (trigger_comment_id = $2 OR $2 = ANY(coalesced_comment_ids))`, fixture.issueID, card.Comment.ID); count != 1 {
		test.Fatalf("card answer runs = %d, want 1", count)
	}
}

// Refusal of one member, then the person answers that member on its card: the
// whole group delivers once, with no provenance rewrite.
func TestDecisionDeliveryMixedGroupRecoversOnCard(test *testing.T) {
	resetDecisionLease(test)
	handler, _ := newDecisionRunnerFixture(test)
	group := readyDecisionDeliveryGroup(test)
	issueID := group.core.issueID
	defer dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, issueID)
	forbidden, permitted := group.records[0], group.records[1]
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, forbidden.Request.ID)
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = ANY($1)`, []string{uuidToString(forbidden.Answer.ID), uuidToString(permitted.Answer.ID)})
	if err := deliverDecisionForTest(handler, group.members[0]); err != nil {
		test.Fatal(err)
	}
	assertWithdrawnForCard(test, forbidden)
	card, err := group.members[0].answerOnCard(decisionCardAnswerInput{Text: "go ahead"})
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, card.Answer.ID)
	for range 2 {
		if err := deliverDecisionForTest(handler, group.members[0]); err != nil {
			test.Fatal(err)
		}
	}
	for _, answerID := range []any{card.Answer.ID, permitted.Answer.ID} {
		if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL AND disclaimed_at IS NULL`, answerID); count != 1 {
			test.Fatal("group member was not delivered after the card answer")
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'running')`, issueID); count != 1 {
		test.Fatalf("group runs = %d, want 1", count)
	}
}

// An unprompted record has no card, so a refusal cancels its request and tells
// the agent to post a new one. Nothing stays open: the issue can complete and
// the lane can be archived.
func TestDecisionDeliveryRefusedUnpromptedRecordIsCancelled(test *testing.T) {
	resetDecisionLease(test)
	handler, _ := newDecisionRunnerFixture(test)
	fixture := newCardFixture(test)
	defer dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, fixture.issueID)
	input := fixture.recordInput()
	input.ParentID, input.EscalationClass = parseUUID(dbfx.Comment(test, fixture.issueID, "An ordinary thread")), "reversible"
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil || recorded.Request.Origin != "unprompted" || recorded.Answer.AnswerSource != "agent_record" {
		test.Fatalf("not a gated unprompted agent record: %+v %v", recorded, err)
	}
	// Creation refuses non-reversible classes, so raise the class after the record exists.
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, recorded.Request.ID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
	before := taskDigest(test, fixture.issueID)
	unprompted := fixture
	unprompted.decision = recorded.Request
	if err := deliverDecisionForTest(handler, unprompted); err != nil {
		test.Fatal(err)
	}
	assertNoDeliveryEffect(test, fixture.issueID, before, recorded)
	assertAnswerWithdrawn(test, recorded, "cancelled")
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND cancel_reason = 'record_disclaimed'`, recorded.Request.ID); count != 1 {
		test.Fatal("refused unprompted record was not cancelled as record_disclaimed")
	}

	// Settle the fixture's own card request, so only the original could block.
	dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'human_removed' WHERE id = $1`, fixture.decision.ID)
	completion := context.WithValue(context.Background(), wakeupActorKey{}, wakeupActor{kind: "agent", id: fixture.agentID})
	if err := guardAgentIssueStatus(completion, testHandler.Queries, db.UpdateIssueParams{ID: parseUUID(fixture.issueID), Status: pgtype.Text{String: "done", Valid: true}}); err != nil {
		test.Fatalf("completion blocked: %v", err)
	}
	if open, err := testHandler.Queries.DecisionOpenLaneExists(context.Background(), db.DecisionOpenLaneExistsParams{WorkspaceID: recorded.Request.WorkspaceID, SessionID: recorded.Request.ChatSessionID}); err != nil || open {
		test.Fatalf("lane archive blocked: open=%v err=%v", open, err)
	}
}
