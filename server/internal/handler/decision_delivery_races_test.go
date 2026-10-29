package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func decisionDeliveryRaceContext(test *testing.T) (context.Context, context.CancelFunc) {
	test.Helper()
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func decisionDeliveryRaceRelease() (<-chan struct{}, func()) {
	release := make(chan struct{})
	var once sync.Once
	return release, func() { once.Do(func() { close(release) }) }
}

func awaitDecisionDeliveryRaceDone(test *testing.T, done <-chan error, name string) error {
	test.Helper()
	t := test
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

func awaitDecisionDeliveryHandlerEvent(test *testing.T, events <-chan decisionDeliveryTransaction, done <-chan error, response *httptest.ResponseRecorder, description string) decisionDeliveryTransaction {
	test.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case event := <-events:
		return event
	case err := <-done:
		test.Fatalf("handler returned before %s: err=%v, HTTP %d: %s", description, err, response.Code, response.Body.String())
	case <-timer.C:
		test.Fatalf("timed out waiting for %s; HTTP %d: %s", description, response.Code, response.Body.String())
	}
	return decisionDeliveryTransaction{}
}

func newDecisionDeliveryRaceWorkspace(test *testing.T) string {
	test.Helper()
	workspaceID := dbfx.Workspace(test, "Decision delivery race", "decision-delivery-race-"+uuid.NewString())
	dbfx.Member(test, workspaceID, testUserID, "owner")
	return workspaceID
}

func moveDecisionDeliveryFixtureToWorkspace(test *testing.T, fixture *askedRecordFixture, workspaceID string) string {
	test.Helper()
	dbfx.Member(test, workspaceID, fixture.humanID, "member")
	runtimeID := dbfx.Insert(test, "agent_runtime", testutil.Cols{
		"workspace_id": workspaceID,
		"name":         "Decision delivery race runtime " + uuid.NewString(),
		"runtime_mode": "local",
		"provider":     "test",
		"status":       "online",
		"owner_id":     testUserID,
	})
	dbfx.Exec(test, `UPDATE agent SET workspace_id = $1, runtime_id = $2 WHERE id = $3`, workspaceID, runtimeID, fixture.agentID)
	dbfx.Exec(test, `UPDATE issue SET workspace_id = $1 WHERE id = $2`, workspaceID, fixture.issueID)
	dbfx.Exec(test, `UPDATE chat_session SET workspace_id = $1 WHERE id = $2`, workspaceID, fixture.sessionID)
	dbfx.Exec(test, `UPDATE comment SET workspace_id = $1 WHERE issue_id = $2`, workspaceID, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_intent SET workspace_id = $1 WHERE issue_id = $2`, workspaceID, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_request SET workspace_id = $1 WHERE issue_id = $2`, workspaceID, fixture.issueID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET runtime_id = $1 WHERE agent_id = $2`, runtimeID, fixture.agentID)
	fixture.issue.WorkspaceID = parseUUID(workspaceID)
	fixture.decision.WorkspaceID = parseUUID(workspaceID)
	return runtimeID
}

func newDecisionDeliveryRaceRuntime(test *testing.T, fixture askedRecordFixture) string {
	test.Helper()
	runtimeID := dbfx.Insert(test, "agent_runtime", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"name":         "Decision delivery race runtime " + uuid.NewString(),
		"runtime_mode": "local",
		"provider":     "test",
		"status":       "online",
		"owner_id":     testUserID,
	})
	dbfx.Exec(test, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, fixture.agentID)
	return runtimeID
}

func newDecisionDeliveryRaceComment(test *testing.T, fixture askedRecordFixture, content string) pgtype.UUID {
	test.Helper()
	commentID := dbfx.Insert(test, "comment", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"issue_id":     fixture.issueID,
		"author_type":  "agent",
		"author_id":    fixture.agentID,
		"parent_id":    fixture.tagID,
		"content":      content,
	})
	return parseUUID(commentID)
}

func newQueuedDecisionDeliveryTask(test *testing.T, fixture askedRecordFixture) pgtype.UUID {
	test.Helper()
	taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{
		"issue_id":           fixture.issueID,
		"status":             "queued",
		"runtime_id":         handlerTestRuntimeID(test),
		"trigger_comment_id": fixture.tagID,
		"comment_thread_id":  fixture.tagID,
	})
	return parseUUID(taskID)
}

func decisionDeliveryInputOccurrences(test *testing.T, taskID, commentID pgtype.UUID) int {
	test.Helper()
	return dbfx.Count(test, `
SELECT count(*)
FROM (
    SELECT trigger_comment_id AS comment_id FROM agent_task_queue WHERE id = $1
    UNION ALL
    SELECT unnest(coalesced_comment_ids) FROM agent_task_queue WHERE id = $1
) AS task_inputs
WHERE comment_id = $2
`, taskID, commentID)
}

func assertDecisionDeliveryAnswer(test *testing.T, answerID pgtype.UUID, outcome string) {
	test.Helper()
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL AND wake_outcome = $2`, answerID, outcome); count != 1 {
		test.Fatalf("acknowledged answers with outcome %q = %d, want 1", outcome, count)
	}
}

func decisionDeliveryTaskStarter(name string, started, committing chan<- decisionDeliveryTransaction, release <-chan struct{}) decisionDeliveryScheduleStarter {
	return decisionDeliveryScheduleStarter{txStarter: testPool, name: name, started: started, committing: committing, release: release}
}

func beginDecisionDeliveryRaceTx(test *testing.T, ctx context.Context) (pgx.Tx, int) {
	test.Helper()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatalf("begin competing transaction: %v", err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = tx.Rollback(context.Background())
		test.Fatalf("read competing transaction pid: %v", err)
	}
	return tx, pid
}

func TestDecisionDeliveryWorkspaceTeardownConcurrent(test *testing.T) {
	for _, order := range []string{"teardown_first", "delivery_first"} {
		test.Run(order, func(test *testing.T) {
			t := test
			workspaceID := newDecisionDeliveryRaceWorkspace(test)
			fixture, recorded := readyDecisionDelivery(test)
			moveDecisionDeliveryFixtureToWorkspace(test, &fixture, workspaceID)
			ctx, cancel := decisionDeliveryRaceContext(test)
			defer cancel()

			deleteResponse := httptest.NewRecorder()
			deleteDone := make(chan error, 1)
			deliveryDone := make(chan error, 1)
			deleteStarted := make(chan decisionDeliveryTransaction, 1)
			deleteCommitting := make(chan decisionDeliveryTransaction, 1)
			deliveryStarted := make(chan decisionDeliveryTransaction, 1)
			deliveryCommitting := make(chan decisionDeliveryTransaction, 1)
			deleteRelease, unblockDelete := decisionDeliveryRaceRelease()
			defer unblockDelete()
			deliveryRelease, unblockDelivery := decisionDeliveryRaceRelease()
			defer unblockDelivery()

			deleteHandler := *testHandler
			deleteHandler.TxStarter = decisionDeliveryTaskStarter("workspace-delete", deleteStarted, deleteCommitting, deleteRelease)
			deleteRequest := newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID, nil)
			deleteRequest.Header.Set("X-Workspace-ID", workspaceID)
			deleteRequest = withURLParam(deleteRequest.WithContext(ctx), "id", workspaceID)

			if order == "teardown_first" {
				go func() {
					deleteHandler.DeleteWorkspace(deleteResponse, deleteRequest)
					deleteDone <- nil
				}()
				deleteTx := awaitDecisionDeliveryEvent(test, deleteCommitting, "workspace teardown commit barrier")
				deliveryHandler, _ := newDecisionRunnerFixture(test)
				deliveryHandler.TxStarter = decisionDeliveryTaskStarter("delivery", deliveryStarted, nil, nil)
				go func() {
					deliveryDone <- deliveryHandler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
				}()
				deliveryTx := awaitDecisionDeliveryEvent(test, deliveryStarted, "delivery transaction start")
				if !waitForDecisionWaiterBlockedBy(test, deliveryTx.pid, deleteTx.pid, 4*time.Second) {
					unblockDelete()
					_ = awaitDecisionDeliveryRaceDone(test, deleteDone, "workspace teardown")
					t.Fatal("delivery did not block on the workspace teardown lock")
				}
				unblockDelete()
				if err := awaitDecisionDeliveryRaceDone(test, deleteDone, "workspace teardown"); err != nil {
					t.Fatalf("workspace teardown returned: %v", err)
				}
				deliveryErr := awaitDecisionDeliveryRaceDone(test, deliveryDone, "delivery after workspace teardown")
				if deliveryErr != nil && !errors.Is(deliveryErr, pgx.ErrNoRows) {
					t.Fatalf("delivery after workspace teardown returned %v", deliveryErr)
				}
			} else {
				deliveryHandler, _ := newDecisionRunnerFixture(test)
				deliveryHandler.TxStarter = decisionDeliveryTaskStarter("delivery", deliveryStarted, deliveryCommitting, deliveryRelease)
				go func() {
					deliveryDone <- deliveryHandler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
				}()
				deliveryTx := awaitDecisionDeliveryEvent(test, deliveryCommitting, "delivery commit barrier")
				go func() {
					deleteHandler.DeleteWorkspace(deleteResponse, deleteRequest)
					deleteDone <- nil
				}()
				// DeleteWorkspace opens a Qoder-cleanup transaction through the same
				// TxStarter before its teardown transaction, so the started event
				// can name that one; match any backend queued behind delivery.
				if !waitForDecisionWaiterBlockedBy(test, 0, deliveryTx.pid, 4*time.Second) {
					unblockDelivery()
					unblockDelete()
					_ = awaitDecisionDeliveryRaceDone(test, deleteDone, "workspace teardown")
					t.Fatal("workspace teardown did not block behind delivery's workspace lock")
				}
				unblockDelivery()
				if err := awaitDecisionDeliveryRaceDone(test, deliveryDone, "decision delivery"); err != nil {
					t.Fatalf("decision delivery returned: %v", err)
				}
				_ = awaitDecisionDeliveryEvent(test, deleteCommitting, "workspace teardown commit barrier")
				assertDecisionDeliveryAnswer(test, recorded.Answer.ID, "run_enqueued")
				var deliveryTaskID pgtype.UUID
				if err := testPool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND (trigger_comment_id = $2 OR $2 = ANY(coalesced_comment_ids))`, fixture.issue.ID, recorded.Comment.ID).Scan(&deliveryTaskID); err != nil {
					t.Fatalf("load committed delivery task before workspace teardown: %v", err)
				}
				unblockDelete()
				if err := awaitDecisionDeliveryRaceDone(test, deleteDone, "workspace teardown"); err != nil {
					t.Fatalf("workspace teardown returned: %v", err)
				}
			}

			if deleteResponse.Code != http.StatusNoContent {
				t.Fatalf("workspace DELETE = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM workspace WHERE id = $1`, workspaceID); count != 0 {
				t.Fatalf("workspace rows after teardown = %d, want 0", count)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1 OR $1 = ANY(coalesced_comment_ids)`, recorded.Comment.ID); count != 0 {
				t.Fatalf("orphaned decision tasks after workspace teardown = %d", count)
			}
		})
	}
}

func TestDecisionDeliveryRuntimeTeardownConcurrent(test *testing.T) {
	for _, order := range []string{"teardown_first", "delivery_first"} {
		test.Run(order, func(test *testing.T) {
			t := test
			fixture, recorded := readyDecisionDelivery(test)
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', completed_at = clock_timestamp() WHERE agent_id = $1`, fixture.agentID)
			runtimeID := newDecisionDeliveryRaceRuntime(test, fixture)
			ctx, cancel := decisionDeliveryRaceContext(test)
			defer cancel()

			deleteResponse := httptest.NewRecorder()
			deleteDone := make(chan error, 1)
			deliveryDone := make(chan error, 1)
			deleteStarted := make(chan decisionDeliveryTransaction, 1)
			deleteCommitting := make(chan decisionDeliveryTransaction, 1)
			deliveryStarted := make(chan decisionDeliveryTransaction, 1)
			deliveryCommitting := make(chan decisionDeliveryTransaction, 1)
			deleteRelease, unblockDelete := decisionDeliveryRaceRelease()
			defer unblockDelete()
			deliveryRelease, unblockDelivery := decisionDeliveryRaceRelease()
			defer unblockDelivery()

			deleteHandler := *testHandler
			deleteHandler.TxStarter = decisionDeliveryTaskStarter("runtime-delete", deleteStarted, deleteCommitting, deleteRelease)
			deleteRequest := newRequest(http.MethodPost, "/api/runtimes/"+runtimeID+"/unbind-agents-and-delete", map[string]any{"expected_active_agent_ids": []string{fixture.agentID}})
			deleteRequest = withURLParam(deleteRequest.WithContext(ctx), "runtimeId", runtimeID)

			if order == "teardown_first" {
				go func() {
					deleteHandler.UnbindAgentsAndDeleteRuntime(deleteResponse, deleteRequest)
					deleteDone <- nil
				}()
				deleteTx := awaitDecisionDeliveryHandlerEvent(test, deleteCommitting, deleteDone, deleteResponse, "runtime teardown commit barrier")
				deliveryHandler, _ := newDecisionRunnerFixture(test)
				deliveryHandler.TxStarter = decisionDeliveryTaskStarter("delivery", deliveryStarted, nil, nil)
				go func() {
					deliveryDone <- deliveryHandler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
				}()
				deliveryTx := awaitDecisionDeliveryEvent(test, deliveryStarted, "delivery transaction start")
				if !waitForDecisionWaiterBlockedBy(test, deliveryTx.pid, deleteTx.pid, 4*time.Second) {
					unblockDelete()
					_ = awaitDecisionDeliveryRaceDone(test, deleteDone, "runtime teardown")
					t.Fatal("delivery did not block on the runtime teardown lock")
				}
				unblockDelete()
				if err := awaitDecisionDeliveryRaceDone(test, deleteDone, "runtime teardown"); err != nil {
					t.Fatalf("runtime teardown returned: %v", err)
				}
				if err := awaitDecisionDeliveryRaceDone(test, deliveryDone, "delivery after runtime teardown"); err != nil {
					t.Fatalf("delivery after runtime teardown returned: %v", err)
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NULL`, recorded.Answer.ID); count != 1 {
					t.Fatalf("runtime teardown acknowledged a deferred answer: pending rows=%d, want 1", count)
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1 OR $1 = ANY(coalesced_comment_ids)`, recorded.Comment.ID); count != 0 {
					t.Fatalf("runtime teardown left %d uncommitted delivery tasks", count)
				}
			} else {
				deliveryHandler, _ := newDecisionRunnerFixture(test)
				deliveryHandler.TxStarter = decisionDeliveryTaskStarter("delivery", deliveryStarted, deliveryCommitting, deliveryRelease)
				go func() {
					deliveryDone <- deliveryHandler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
				}()
				deliveryTx := awaitDecisionDeliveryEvent(test, deliveryCommitting, "delivery commit barrier")
				go func() {
					deleteHandler.UnbindAgentsAndDeleteRuntime(deleteResponse, deleteRequest)
					deleteDone <- nil
				}()
				deleteTx := awaitDecisionDeliveryHandlerEvent(test, deleteStarted, deleteDone, deleteResponse, "runtime teardown transaction start")
				if !waitForDecisionWaiterBlockedBy(test, deleteTx.pid, deliveryTx.pid, 4*time.Second) {
					unblockDelivery()
					unblockDelete()
					_ = awaitDecisionDeliveryRaceDone(test, deleteDone, "runtime teardown")
					t.Fatal("runtime teardown did not block behind delivery's runtime lock")
				}
				unblockDelivery()
				if err := awaitDecisionDeliveryRaceDone(test, deliveryDone, "decision delivery"); err != nil {
					t.Fatalf("decision delivery returned: %v", err)
				}
				_ = awaitDecisionDeliveryEvent(test, deleteCommitting, "runtime teardown commit barrier")
				assertDecisionDeliveryAnswer(test, recorded.Answer.ID, "run_enqueued")
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1 OR $1 = ANY(coalesced_comment_ids)`, recorded.Comment.ID); count != 1 {
					t.Fatalf("delivery task rows before runtime teardown = %d, want 1", count)
				}
				unblockDelete()
				if err := awaitDecisionDeliveryRaceDone(test, deleteDone, "runtime teardown"); err != nil {
					t.Fatalf("runtime teardown returned: %v", err)
				}
			}

			if deleteResponse.Code != http.StatusOK {
				t.Fatalf("runtime DELETE = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_runtime WHERE id = $1`, runtimeID); count != 0 {
				t.Fatalf("runtime rows after teardown = %d, want 0", count)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent WHERE id = $1 AND runtime_id IS NULL`, fixture.agentID); count != 1 {
				t.Fatalf("unbound agent rows after teardown = %d, want 1", count)
			}
			if order == "delivery_first" {
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE trigger_comment_id = $1 OR $1 = ANY(coalesced_comment_ids)`, recorded.Comment.ID); count != 1 {
					t.Fatalf("delivery task records after runtime teardown = %d, want 1", count)
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE (trigger_comment_id = $1 OR $1 = ANY(coalesced_comment_ids)) AND runtime_id IS NOT NULL`, recorded.Comment.ID); count != 0 {
					t.Fatalf("task records still reference deleted runtime = %d", count)
				}
				assertDecisionDeliveryAnswer(test, recorded.Answer.ID, "run_enqueued")
			} else if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NULL`, recorded.Answer.ID); count != 1 {
				t.Fatalf("runtime-first delivery did not preserve pending answer: %d", count)
			}
		})
	}
}

func TestDecisionDeliveryDelegatedFailureMergeConcurrent(test *testing.T) {
	t := test
	fixture, recorded := readyDecisionDelivery(test)
	taskID := newQueuedDecisionDeliveryTask(test, fixture)
	recoveryCommentID := newDecisionDeliveryRaceComment(test, fixture, "Delegated failure recovery")
	ctx, cancel := decisionDeliveryRaceContext(test)
	defer cancel()
	release, unblock := decisionDeliveryRaceRelease()
	defer unblock()
	started := make(chan decisionDeliveryTransaction, 1)
	committing := make(chan decisionDeliveryTransaction, 1)
	handler, _ := newDecisionRunnerFixture(test)
	handler.TxStarter = decisionDeliveryTaskStarter("delivery", started, committing, release)
	deliveryDone := make(chan error, 1)
	go func() {
		deliveryDone <- handler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
	}()
	deliveryTx := awaitDecisionDeliveryEvent(test, committing, "decision delivery commit barrier")
	recoveryTx, recoveryPID := beginDecisionDeliveryRaceTx(test, ctx)
	recoveryDone := make(chan error, 1)
	go func() {
		_, err := testHandler.Queries.WithTx(recoveryTx).MergeDelegatedFailureCommentIntoPendingTask(ctx, db.MergeDelegatedFailureCommentIntoPendingTaskParams{
			CommentID:      recoveryCommentID,
			TriggerSummary: pgtype.Text{String: "delegated failure recovery", Valid: true},
			IssueID:        fixture.issue.ID,
			AgentID:        parseUUID(fixture.agentID),
		})
		if err == nil {
			err = recoveryTx.Commit(ctx)
		} else {
			_ = recoveryTx.Rollback(context.Background())
		}
		recoveryDone <- err
	}()
	if !waitForDecisionWaiterBlockedBy(test, recoveryPID, deliveryTx.pid, 4*time.Second) {
		unblock()
		_ = awaitDecisionDeliveryRaceDone(test, deliveryDone, "decision delivery")
		_ = awaitDecisionDeliveryRaceDone(test, recoveryDone, "delegated failure recovery")
		t.Fatal("delegated failure merge did not overlap the delivery row lock")
	}
	unblock()
	if err := awaitDecisionDeliveryRaceDone(test, deliveryDone, "decision delivery"); err != nil {
		t.Fatalf("decision delivery returned: %v", err)
	}
	if err := awaitDecisionDeliveryRaceDone(test, recoveryDone, "delegated failure recovery"); err != nil {
		t.Fatalf("delegated failure recovery returned: %v", err)
	}
	assertDecisionDeliveryAnswer(test, recorded.Answer.ID, "merged_into_queued")
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, taskID); count != 1 {
		t.Fatalf("queued task rows after both merges = %d, want 1", count)
	}
	for _, commentID := range []pgtype.UUID{recorded.Comment.ID, recoveryCommentID} {
		if count := decisionDeliveryInputOccurrences(test, taskID, commentID); count != 1 {
			t.Fatalf("task input %s occurs %d times, want 1", uuidToString(commentID), count)
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, fixture.agentID); count != 1 {
		t.Fatalf("queued runs after recovery/delivery = %d, want 1", count)
	}
}

func TestDecisionDeliveryQueuedMergeConcurrent(test *testing.T) {
	fixture, recorded := readyDecisionDelivery(test)
	taskID := newQueuedDecisionDeliveryTask(test, fixture)
	mergeCommentID := newDecisionDeliveryRaceComment(test, fixture, "Concurrent queued comment")
	issue, err := testHandler.Queries.GetIssue(context.Background(), fixture.issue.ID)
	if err != nil {
		test.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(fixture.agentID))
	if err != nil {
		test.Fatal(err)
	}
	mergeParams, _, err := testHandler.commentMergeParams(context.Background(), issue, commentAgentTrigger{Agent: agent, Source: commentTriggerSourceIssueAssignee}, mergeCommentID, pgtype.Text{})
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := decisionDeliveryRaceContext(test)
	defer cancel()
	release, unblock := decisionDeliveryRaceRelease()
	defer unblock()
	started := make(chan decisionDeliveryTransaction, 1)
	committing := make(chan decisionDeliveryTransaction, 1)
	handler, _ := newDecisionRunnerFixture(test)
	handler.TxStarter = decisionDeliveryTaskStarter("delivery", started, committing, release)
	deliveryDone := make(chan error, 1)
	go func() {
		deliveryDone <- handler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
	}()
	deliveryTx := awaitDecisionDeliveryEvent(test, committing, "decision delivery commit barrier")
	mergeTx, mergePID := beginDecisionDeliveryRaceTx(test, ctx)
	mergeDone := make(chan error, 1)
	go func() {
		_, err := testHandler.Queries.WithTx(mergeTx).MergeCommentIntoQueuedIssueTask(ctx, db.MergeCommentIntoQueuedIssueTaskParams{
			NewTriggerCommentID:     mergeParams.NewTriggerCommentID,
			NewTriggerSummary:       mergeParams.NewTriggerSummary,
			NewOriginatorUserID:     mergeParams.NewOriginatorUserID,
			NewAccountableUserID:    mergeParams.NewAccountableUserID,
			NewOriginatorSource:     mergeParams.NewOriginatorSource,
			NewDelegatedFromTaskID:  mergeParams.NewDelegatedFromTaskID,
			NewRuleVersionID:        mergeParams.NewRuleVersionID,
			NewTriggerEvidenceKind:  mergeParams.NewTriggerEvidenceKind,
			NewTriggerEvidenceRefID: mergeParams.NewTriggerEvidenceRefID,
			NewRuntimeMcpOverlay:    mergeParams.NewRuntimeMcpOverlay,
			NewRuntimeConnectedApps: mergeParams.NewRuntimeConnectedApps,
			IssueID:                 mergeParams.IssueID,
			AgentID:                 mergeParams.AgentID,
			TargetTaskID:            taskID,
			HeadSha:                 mergeParams.HeadSha,
		})
		if err == nil {
			err = mergeTx.Commit(ctx)
		} else {
			_ = mergeTx.Rollback(context.Background())
		}
		mergeDone <- err
	}()
	if !waitForDecisionWaiterBlockedBy(test, mergePID, deliveryTx.pid, 4*time.Second) {
		unblock()
		_ = awaitDecisionDeliveryRaceDone(test, deliveryDone, "decision delivery")
		_ = awaitDecisionDeliveryRaceDone(test, mergeDone, "queued comment merge")
		test.Fatal("queued comment merge did not overlap the delivery row lock")
	}
	unblock()
	if err := awaitDecisionDeliveryRaceDone(test, deliveryDone, "decision delivery"); err != nil {
		test.Fatalf("decision delivery returned: %v", err)
	}
	if err := awaitDecisionDeliveryRaceDone(test, mergeDone, "queued comment merge"); err != nil {
		test.Fatalf("queued comment merge returned: %v", err)
	}
	assertDecisionDeliveryAnswer(test, recorded.Answer.ID, "merged_into_queued")
	for _, commentID := range []pgtype.UUID{recorded.Comment.ID, mergeCommentID} {
		if count := decisionDeliveryInputOccurrences(test, taskID, commentID); count != 1 {
			test.Fatalf("task input %s occurs %d times, want 1", uuidToString(commentID), count)
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, fixture.agentID); count != 1 {
		test.Fatalf("queued runs after merge/delivery = %d, want 1", count)
	}
}

func TestDecisionDeliveryPlannedRegistrationConcurrent(test *testing.T) {
	fixture, recorded := readyDecisionDelivery(test)
	taskIDString := dbfx.Task(test, fixture.agentID, testutil.Cols{
		"issue_id":           fixture.issueID,
		"status":             "running",
		"runtime_id":         handlerTestRuntimeID(test),
		"trigger_comment_id": fixture.tagID,
		"comment_thread_id":  fixture.tagID,
	})
	taskID := parseUUID(taskIDString)
	dbfx.Exec(test, `UPDATE agent_task_queue SET delivered_comment_ids = ARRAY[trigger_comment_id] WHERE id = $1`, taskIDString)
	ctx, cancel := decisionDeliveryRaceContext(test)
	defer cancel()
	release, unblock := decisionDeliveryRaceRelease()
	defer unblock()
	started := make(chan decisionDeliveryTransaction, 1)
	committing := make(chan decisionDeliveryTransaction, 1)
	handler, _ := newDecisionRunnerFixture(test)
	handler.TxStarter = decisionDeliveryTaskStarter("delivery", started, committing, release)
	deliveryDone := make(chan error, 1)
	go func() {
		deliveryDone <- handler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
	}()
	deliveryTx := awaitDecisionDeliveryEvent(test, committing, "decision delivery commit barrier")
	registrationTx, registrationPID := beginDecisionDeliveryRaceTx(test, ctx)
	registrationDone := make(chan error, 1)
	go func() {
		_, err := testHandler.Queries.WithTx(registrationTx).RegisterPlannedCommentForActiveTask(ctx, db.RegisterPlannedCommentForActiveTaskParams{
			CommentID:    recorded.Comment.ID,
			IssueID:      fixture.issue.ID,
			AgentID:      parseUUID(fixture.agentID),
			TargetTaskID: taskID,
		})
		if err == nil {
			err = registrationTx.Commit(ctx)
		} else {
			_ = registrationTx.Rollback(context.Background())
		}
		registrationDone <- err
	}()
	if !waitForDecisionWaiterBlockedBy(test, registrationPID, deliveryTx.pid, 4*time.Second) {
		unblock()
		_ = awaitDecisionDeliveryRaceDone(test, deliveryDone, "decision delivery")
		_ = awaitDecisionDeliveryRaceDone(test, registrationDone, "planned input registration")
		test.Fatal("planned input registration did not overlap the delivery row lock")
	}
	unblock()
	if err := awaitDecisionDeliveryRaceDone(test, deliveryDone, "decision delivery"); err != nil {
		test.Fatalf("decision delivery returned: %v", err)
	}
	if err := awaitDecisionDeliveryRaceDone(test, registrationDone, "planned input registration"); err != nil {
		test.Fatalf("planned input registration returned: %v", err)
	}
	assertDecisionDeliveryAnswer(test, recorded.Answer.ID, "planned_input")
	if count := decisionDeliveryInputOccurrences(test, taskID, recorded.Comment.ID); count != 1 {
		test.Fatalf("planned decision input occurrences = %d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND NOT ($2 = ANY(delivered_comment_ids))`, taskID, recorded.Comment.ID); count != 1 {
		test.Fatal("planned decision input was marked delivered on the active task")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')`, fixture.issueID, fixture.agentID); count != 1 {
		test.Fatalf("active tasks after planned registration = %d, want 1", count)
	}
}
