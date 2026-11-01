package handler

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// taskDigest fingerprints every task and every wakeup receipt that the delivery
// itself captures (tagged with the answer); the receipt of a withdrawal
// comment's own insert is a plain comment event and stays out.
func taskDigest(test *testing.T, issueID string) string {
	test.Helper()
	var digest string
	dbfx.QueryRow(test, `SELECT md5(COALESCE((SELECT string_agg(t::text, '|' ORDER BY t.id) FROM agent_task_queue t WHERE t.issue_id = $1), '')
		|| COALESCE((SELECT string_agg(r.id::text, '|' ORDER BY r.id) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id = r.wakeup_id WHERE w.issue_id = $1 AND r.payload ? 'decision_answer_id'), ''))`, issueID).Scan(&digest)
	return digest
}

func TestDecisionDeliveryRefusesAgentRecordOfGatedClass(test *testing.T) {
	for _, branch := range []string{"enqueue", "queued", "running", "wakeup", "group"} {
		test.Run(branch, func(test *testing.T) {
			resetDecisionLease(test)
			handler, _ := newDecisionRunnerFixture(test)
			var issueID string
			// Leftover runs would be claimed by later tests on the shared runtime.
			defer func() { dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE status IN ('queued', 'running') AND issue_id = $1`, issueID) }()
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
