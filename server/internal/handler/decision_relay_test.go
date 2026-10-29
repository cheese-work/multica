package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func decisionRelaySubscription(test *testing.T, fixture askedRecordFixture, humanID string) db.IssueWakeup {
	test.Helper()
	wakeups := &service.IssueWakeupService{Tasks: testHandler.TaskService}
	wakeup, err := wakeups.Create(context.Background(), fixture.issue.ID, parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
		AgentID: fixture.agentID, Kind: "event", Mode: "once", EventTypes: []string{"comment.created"},
		FilterActorType: "member", FilterActorID: humanID, Instruction: "Relay the human's answer to the waiting owner.",
	})
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Cleanup(test, `DELETE FROM issue_wakeup WHERE id = $1`, wakeup.ID)
	dbfx.Cleanup(test, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID)
	return wakeup
}

func TestDecisionRelayCapturesHumanAnswerOnlyWhenEffective(test *testing.T) {
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	wakeup := decisionRelaySubscription(test, fixture, fixture.humanID)
	other := decisionRelaySubscription(test, fixture, testUserID)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID); count != 0 {
		test.Fatalf("pending answer captured %d receipts", count)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND payload->>'comment_id' = $2 AND payload->>'actor_type' = 'member' AND payload->>'actor_id' = $3`, wakeup.ID, uuidToString(recorded.Comment.ID), fixture.humanID); count != 1 {
		test.Fatalf("effective human answer captured %d receipts, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1`, other.ID); count != 0 {
		test.Fatalf("another person's subscription captured %d receipts", count)
	}
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID); count != 1 {
		test.Fatalf("retry changed receipt count to %d", count)
	}
}

func TestDecisionRelayPrefersAskerAndFallsBackWhenGone(test *testing.T) {
	for _, archived := range []bool{false, true} {
		test.Run(map[bool]string{false: "asker", true: "assignee_fallback"}[archived], func(test *testing.T) {
			resetDecisionLease(test)
			fixture, recorded := readyDecisionDelivery(test)
			assignee := createHandlerTestAgent(test, "decision-relay-owner", []byte("[]"))
			dbfx.Exec(test, `UPDATE issue SET assignee_id = $1 WHERE id = $2`, assignee, fixture.issueID)
			expected := fixture.agentID
			if archived {
				dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
				expected = assignee
			}
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND trigger_comment_id = $3`, fixture.issueID, expected, recorded.Comment.ID); count != 1 {
				test.Fatalf("recipient %s has %d admitted runs, want 1", expected, count)
			}
		})
	}
}

func TestCHE2199DecisionRelayReplay(test *testing.T) {
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	fixture.humanID = testUserID
	dbfx.Exec(test, `UPDATE chat_session SET creator_id = $1 WHERE id = $2`, testUserID, fixture.sessionID)
	wakeup := decisionRelaySubscription(test, fixture, fixture.humanID)
	asker := fixture.agentID
	recorder := createHandlerTestAgent(test, "decision-dm-recorder", []byte("[]"))
	parent := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "agent", "author_id": asker, "content": "F4: waive the 8 retrospective tests, or do not waive?"})
	chatTask := dbfx.Task(test, recorder, testutil.Cols{"status": "running", "chat_session_id": fixture.sessionID, "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Exec(test, `UPDATE chat_session SET agent_id = $1 WHERE id = $2`, recorder, fixture.sessionID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, chatTask)
	request := fixture.recordRequest()
	request.Header.Set("X-Agent-ID", recorder)
	request.Header.Set("X-Task-ID", chatTask)
	assignee := createHandlerTestAgent(test, "decision-current-assignee", []byte("[]"))
	dbfx.Exec(test, `UPDATE issue SET assignee_id = $1 WHERE id = $2`, assignee, fixture.issueID)
	input := fixture.recordInput()
	input.ParentID, input.EscalationClass = parseUUID(parent), "product_direction"
	recorded, err := testHandler.createAskedDecisionRecord(request, fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID); count != 0 {
		test.Fatalf("pending record captured %d receipts", count)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2, $3)`, fixture.taskID, fixture.chatTaskID, chatTask)
	dbfx.Exec(test, `UPDATE decision_answer SET effective_at = clock_timestamp() - interval '1 second' WHERE id = $1`, recorded.Answer.ID)
	decisionPass(test, runner)
	wakeups := &service.IssueWakeupService{Tasks: testHandler.TaskService}
	if err := wakeups.Tick(context.Background()); err != nil {
		test.Fatal(err)
	}
	var fires int32
	dbfx.QueryRow(test, `SELECT fire_count FROM issue_wakeup WHERE id = $1`, wakeup.ID).Scan(&fires)
	if fires != 1 {
		test.Fatalf("CHE-2199 replay fire_count=%d, want 1", fires)
	}
	ownerRuns := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, asker)
	if ownerRuns == 0 {
		test.Fatal("CHE-2199 replay admitted no owner run")
	}
	decisionPass(test, runner)
	if err := wakeups.Tick(context.Background()); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, asker); count != ownerRuns {
		test.Fatalf("retry admitted %d owner runs, previously %d", count, ownerRuns)
	}
	claimed, body := claimTaskByRuntimeForTest(test, handlerTestRuntimeID(test))
	if claimed == nil {
		test.Fatalf("owner run was not claimable: %s", body)
	}
	start := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+claimed.ID+"/start", map[string]any{"capabilities": []string{protocol.DaemonCapabilityTaskSupplementV1}})
	start = withURLParam(start, "taskId", claimed.ID)
	testutil.Call(test, testHandler.StartTask, start).Want(http.StatusOK)
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND issue_id = $2 AND agent_id = $3 AND status = 'running'`, claimed.ID, fixture.issueID, asker); count != 1 {
		test.Fatal("the owner run did not reach running")
	}
	test.Logf("CHE-2199 replay: ordinary F4 ask -> agent DM record -> completed answering turn -> fire_count=%d -> owner runs=%d -> claimed -> running; no manual relay; retry admits no additional runs", fires, ownerRuns)
}
