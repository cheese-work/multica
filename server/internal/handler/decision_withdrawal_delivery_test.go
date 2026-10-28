package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionRetractionFollowsDecisionRecipient(test *testing.T) {
	for _, assigneeType := range []string{"agent", "squad", "member", "unassigned", "archived_asker"} {
		test.Run(assigneeType, func(test *testing.T) {
			resetDecisionLease(test)
			fixture, recorded := readyDecisionDelivery(test)
			assignee := createHandlerTestAgent(test, "Retraction fallback owner", []byte("[]"))
			dbfx.Exec(test, `UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, assignee, fixture.issueID)
			switch assigneeType {
			case "squad":
				squad := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Retraction fallback squad", "creator_id": testUserID, "leader_id": assignee})
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squad, fixture.issueID)
			case "member":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'member', assignee_id = $1 WHERE id = $2`, testUserID, fixture.issueID)
			case "unassigned":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = NULL, assignee_id = NULL WHERE id = $1`, fixture.issueID)
			}
			handler, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND trigger_comment_id = $3`, fixture.issueID, fixture.agentID, recorded.Comment.ID); count != 1 {
				test.Fatalf("decision reached asker %d times, want 1", count)
			}
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, fixture.agentID)
			expected := fixture.agentID
			if assigneeType == "archived_asker" {
				dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
				expected = assignee
			}
			withdrawn, err := handler.disclaimDecisionAnswerTx(fixture.request(), fixture.issue.WorkspaceID, recorded.Answer.ID, parseUUID(fixture.humanID))
			if err != nil {
				test.Fatal(err)
			}
			decisionPass(test, runner)
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND trigger_comment_id = $3`, fixture.issueID, expected, withdrawn.Answer.WithdrawalCommentID); count != 1 {
				test.Fatalf("retraction reached expected decision recipient %d times, want 1", count)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id <> $2 AND trigger_comment_id = $3`, fixture.issueID, expected, withdrawn.Answer.WithdrawalCommentID); count != 0 {
				test.Fatalf("retraction reached %d unrelated recipients", count)
			}
			decisionPass(test, runner)
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2`, fixture.issueID, withdrawn.Answer.WithdrawalCommentID); count != 1 {
				test.Fatalf("retry changed retraction run count to %d, want 1", count)
			}
		})
	}
}

func TestDecisionWithdrawalDelivery(test *testing.T) {
	for _, branch := range []string{"enqueue", "queued", "running", "member", "unassigned", "unbound", "reassigned", "veto_suppressed", "not_woken", "closed", "archived_lane"} {
		test.Run(branch, func(test *testing.T) {
			resetDecisionLease(test)
			fixture, recorded := readyDecisionDelivery(test)
			handler, runner := newDecisionRunnerFixture(test)
			if branch != "not_woken" {
				dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, recorded.Answer.ID)
				dbfx.Exec(test, `UPDATE decision_intent SET group_woken_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
			}
			if branch == "archived_lane" {
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			}
			withdrawn, err := handler.disclaimDecisionAnswerTx(fixture.request(), fixture.issue.WorkspaceID, recorded.Answer.ID, parseUUID(fixture.humanID))
			if err != nil {
				test.Fatal(err)
			}
			outcome := "run_enqueued"
			recipient := fixture.agentID
			switch branch {
			case "queued", "running":
				active := dbfx.Task(test, recipient, testutil.Cols{"issue_id": fixture.issueID, "status": branch, "runtime_id": handlerTestRuntimeID(test)})
				dbfx.Exec(test, `UPDATE agent_task_queue SET comment_thread_id = $1 WHERE id = $2`, fixture.tagID, active)
				outcome = "merged_into_queued"
				if branch == "running" {
					outcome = "planned_input"
				}
			case "member":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'member', assignee_id = $1 WHERE id = $2`, testUserID, fixture.issueID)
			case "unassigned":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = NULL, assignee_id = NULL WHERE id = $1`, fixture.issueID)
			case "unbound":
				dbfx.Exec(test, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, recipient)
				outcome = ""
			case "reassigned", "veto_suppressed":
				assignee := createHandlerTestAgent(test, "Withdrawal assignee", []byte("[]"))
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, assignee, fixture.issueID)
				if branch == "veto_suppressed" {
					dbfx.Exec(test, `UPDATE decision_answer SET retraction_delivered_at = clock_timestamp(), retraction_outcome = 'suppressed_agent_veto' WHERE id = $1`, recorded.Answer.ID)
					outcome = "suppressed_agent_veto"
				}
			case "not_woken":
				outcome = ""
			case "closed":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
				outcome = "skipped_ineligible"
			}
			sessions := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE workspace_id = $1`, testWorkspaceID)
			pings := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID)
			decisionPass(test, runner)
			decisionPass(test, runner)
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND COALESCE(retraction_outcome, '') = $2 AND (retraction_delivered_at IS NOT NULL) = ($2 <> '')`, recorded.Answer.ID, outcome); count != 1 {
				test.Fatalf("withdrawal acknowledgement mismatch, want %q", outcome)
			}
			if outcome == "run_enqueued" || outcome == "merged_into_queued" || outcome == "planned_input" {
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fixture.issueID, recipient, withdrawn.Answer.WithdrawalCommentID); count != 1 {
					test.Fatalf("withdrawal did not reach the asker exactly once: %d", count)
				}
			}
			if branch == "archived_lane" {
				if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'lane_archived' AND reask_delivered_at IS NOT NULL AND reask_outcome IN ('run_enqueued', 'merged_into_queued')`, fixture.decision.ID); count != 1 {
					test.Fatal("archived-lane withdrawal did not durably re-ask")
				}
			}
			if dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE workspace_id = $1`, testWorkspaceID) != sessions || dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID) != pings {
				test.Fatal("withdrawal automatically re-pinged or created a DM")
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'delivery'`, recorded.Answer.ID); count != 0 {
				test.Fatal("withdrawal posted a redundant delivery comment")
			}
			if branch == "veto_suppressed" && dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, fixture.issueID) != 0 {
				test.Fatal("reassignment replayed a persisted veto suppression")
			}
		})
	}
}

func TestDecisionRetractionRollback(test *testing.T) {
	resetDecisionLease(test)
	fixture, recorded := readyDecisionDelivery(test)
	dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, recorded.Answer.ID)
	if _, err := testHandler.disclaimDecisionAnswerTx(fixture.request(), fixture.issue.WorkspaceID, recorded.Answer.ID, parseUUID(fixture.humanID)); err != nil {
		test.Fatal(err)
	}
	handler, runner := newDecisionRunnerFixture(test)
	handler.TxStarter = decisionFailCommit{testPool}
	_, _ = runner.ProcessPass(context.Background())
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND retraction_delivered_at IS NOT NULL`, recorded.Answer.ID) != 0 || dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, fixture.issueID) != 0 {
		test.Fatal("rolled-back retraction escaped its transaction")
	}
	handler.TxStarter = testPool
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND retraction_delivered_at IS NOT NULL`, recorded.Answer.ID); count != 1 {
		test.Fatal("retraction did not recover after rollback")
	}
}

func TestDecisionRetractionRechecksAssigneeAndRuntime(test *testing.T) {
	fixture, recorded := readyDecisionDelivery(test)
	dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, recorded.Answer.ID)
	withdrawn, err := testHandler.disclaimDecisionAnswerTx(fixture.request(), fixture.issue.WorkspaceID, recorded.Answer.ID, parseUUID(fixture.humanID))
	if err != nil {
		test.Fatal(err)
	}
	recipient := createHandlerTestAgent(test, "Changed withdrawal recipient", []byte("[]"))
	runtimeID := dbfx.Insert(test, "agent_runtime", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Changed withdrawal runtime", "runtime_mode": "local", "provider": "test", "status": "online", "owner_id": testUserID})
	handler, _ := newDecisionRunnerFixture(test)
	handler.TxStarter = &decisionDeliveryMutation{txStarter: testPool, mutate: func() {
		dbfx.Exec(test, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, recipient)
		dbfx.Exec(test, `UPDATE issue SET assignee_id = $1 WHERE id = $2`, recipient, fixture.issueID)
		dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
	}}
	if err := handler.deliverDecisionItem(context.Background(), fixture.issue.WorkspaceID, recorded.Answer.ID, "retraction"); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND runtime_id = $3 AND trigger_comment_id = $4`, fixture.issueID, recipient, runtimeID, withdrawn.Answer.WithdrawalCommentID); count != 1 {
		test.Fatal("retraction used its stale owner/runtime snapshot")
	}
}
