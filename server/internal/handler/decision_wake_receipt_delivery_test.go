package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2394 P3a: deliver one receipt (queued, running, idle, skipped_ineligible).

func receiptIDFor(test *testing.T, request pgtype.UUID, agentID string) pgtype.UUID {
	test.Helper()
	var id pgtype.UUID
	dbfx.QueryRow(test, `SELECT r.id FROM decision_wake_receipt r JOIN decision_wake_event e ON e.id = r.event_id
		WHERE e.request_id = $1 AND r.recipient_agent_id = $2`, request, agentID).Scan(&id)
	return id
}

type receiptState struct {
	Status, Outcome string
	DeliveredAt     pgtype.Timestamptz
}

func receiptStateOf(test *testing.T, id pgtype.UUID) receiptState {
	test.Helper()
	var state receiptState
	dbfx.QueryRow(test, `SELECT status, coalesce(outcome, ''), delivered_at FROM decision_wake_receipt WHERE id = $1`, id).Scan(&state.Status, &state.Outcome, &state.DeliveredAt)
	return state
}

func TestDeliverDecisionWakeReceiptByRecipientTaskState(test *testing.T) {
	for _, state := range []string{"queued", "running", "idle"} {
		test.Run(state, func(test *testing.T) {
			fixture, recorded := answeredActionFixture(test)
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
			finalizeAnswerWake(test, fixture)
			want := map[string]string{"queued": "merged_into_queued", "running": "planned_input", "idle": "run_enqueued"}[state]
			var active string
			if state != "idle" {
				active = dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": state, "runtime_id": handlerTestRuntimeID(test)})
				dbfx.Exec(test, `UPDATE agent_task_queue SET comment_thread_id = $1 WHERE id = $2`, fixture.tagID, active)
			}
			receipt := receiptIDFor(test, fixture.decision.ID, fixture.agentID)
			before := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running')`, fixture.issueID, fixture.agentID)
			if err := testHandler.deliverDecisionWakeReceipt(context.Background(), receipt); err != nil {
				test.Fatal(err)
			}
			got := receiptStateOf(test, receipt)
			if got.Status != "delivered" || got.Outcome != want || !got.DeliveredAt.Valid {
				test.Fatalf("receipt = %+v, want delivered with outcome %s", got, want)
			}
			after := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running')`, fixture.issueID, fixture.agentID)
			if wantAfter := before + map[bool]int{true: 1}[state == "idle"]; after != wantAfter {
				test.Fatalf("active tasks = %d, want %d", after, wantAfter)
			}
			if state != "idle" {
				if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND handoff_note LIKE '%' || $2 || '%'`, active, uuidToString(recorded.Answer.IssueCommentID)); n != 1 {
					test.Fatalf("task %s did not receive the answer comment as input", active)
				}
			}
		})
	}
}

func TestDeliverDecisionWakeReceiptSkipsArchivedRecipientAndDeliversOther(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	leader := dbfx.Agent(test, "wake receipt leader", handlerTestRuntimeID(test))
	squadID := dbfx.Squad(test, "Wake delivery", leader)
	dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
	if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	finalizeAnswerWake(test, fixture)
	dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, leader)
	for _, agent := range []string{leader, fixture.agentID} {
		if err := testHandler.deliverDecisionWakeReceipt(context.Background(), receiptIDFor(test, fixture.decision.ID, agent)); err != nil {
			test.Fatal(err)
		}
	}
	if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, leader)); got.Status != "skipped_ineligible" || got.Outcome != "" {
		test.Fatalf("archived leader receipt = %+v, want skipped_ineligible without outcome", got)
	}
	if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, fixture.agentID)); got.Status != "delivered" || got.Outcome != "run_enqueued" {
		test.Fatalf("requester receipt = %+v, want delivered run_enqueued", got)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, leader); n != 0 {
		test.Fatalf("archived leader got %d queued tasks", n)
	}
}
