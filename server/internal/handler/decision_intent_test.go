package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type intentFixture struct {
	agentID, issueID, taskID, humanID string
}

func newIntentFixture(test *testing.T) intentFixture {
	test.Helper()
	agentID := createHandlerTestAgent(test, "decision-intent-"+uuid.NewString()[:8], []byte("[]"))
	issueID := dbfx.Issue(test, "intent issue", testutil.Cols{"assignee_type": "agent", "assignee_id": agentID})
	taskID := dbfx.Task(test, agentID, testutil.Cols{"issue_id": issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	humanID := dbfx.User(test, "Asked human", "intent-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": humanID, "role": "member"})
	test.Cleanup(func() {
		_, err := testPool.Exec(context.Background(), `DELETE FROM decision_intent WHERE issue_id = $1`, issueID)
		if err != nil {
			test.Error(err)
		}
	})
	return intentFixture{agentID, issueID, taskID, humanID}
}

func (fixture intentFixture) mention() string {
	return "[@Human](mention://member/" + fixture.humanID + ")"
}

func (fixture intentFixture) post(test *testing.T, source, taskID, content string, extra map[string]any) *testutil.Response {
	test.Helper()
	body := map[string]any{"content": content, "decision_intent": true}
	for key, value := range extra {
		body[key] = value
	}
	request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+fixture.issueID+"/comments", body), "id", fixture.issueID)
	request.Header.Set("X-Actor-Source", source)
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", taskID)
	return testutil.Call(test, testHandler.CreateComment, request)
}

func TestDecisionIntentIssueDeletedBeforeLock(test *testing.T) {
	fixture := newIntentFixture(test)
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(fixture.issueID))
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `DELETE FROM issue WHERE id = $1`, fixture.issueID)
	tx, err := decisiontx.Begin(ctx, testPool, parseUUID(testWorkspaceID))
	if err != nil {
		test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	request := newRequest(http.MethodPost, "/", nil)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Task-ID", fixture.taskID)
	_, targets, err := testHandler.prepareDecisionIntent(request, testHandler.Queries.WithTx(tx), issue, "agent", parseUUID(fixture.agentID), fixture.mention())
	if err == nil || len(targets) != 0 {
		test.Fatalf("deleted issue was accepted: targets=%v err=%v", targets, err)
	}
	response := httptest.NewRecorder()
	writeDecisionIntentError(response, err)
	if response.Code != http.StatusNotFound {
		test.Fatalf("deleted issue status = %d, want 404; body=%s", response.Code, response.Body.String())
	}
}

func TestDecisionIntentAllowAndStamp(test *testing.T) {
	for _, squad := range []bool{false, true} {
		test.Run(fmt.Sprintf("squad=%t", squad), func(test *testing.T) {
			fixture := newIntentFixture(test)
			if squad {
				squadID := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Intent squad", "creator_id": testUserID, "leader_id": fixture.agentID})
				dbfx.Exec(test, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, squadID, fixture.agentID)
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
			}
			var response CommentResponse
			fixture.post(test, "task_token", fixture.taskID, fixture.mention(), nil).Want(http.StatusCreated).JSON(&response)
			var intentID, taskID, agentID string
			var openedAt, expiresAt time.Time
			dbfx.QueryRow(test, `SELECT intent.id, intent.task_id, intent.agent_id, intent.opened_at, intent.expires_at FROM decision_intent intent JOIN comment ON comment.decision_intent_id = intent.id WHERE comment.id = $1 AND intent.comment_id = comment.id`, response.ID).Scan(&intentID, &taskID, &agentID, &openedAt, &expiresAt)
			if taskID != fixture.taskID || agentID != fixture.agentID || expiresAt.Sub(openedAt) != 3*time.Minute || response.Type != "comment" {
				test.Fatalf("wrong stamp/window: task=%s agent=%s window=%s type=%s", taskID, agentID, expiresAt.Sub(openedAt), response.Type)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1 AND human_user_id = $2`, intentID, fixture.humanID); count != 1 {
				test.Fatalf("target count = %d", count)
			}
		})
	}
}

func TestDecisionIntentRefusalsCreateNothing(test *testing.T) {
	for _, refusal := range []string{"chat", "other_issue", "other_agent", "non_assignee", "stopped", "terminal", "pat", "cloud_pat", "interactive_jwt", "cli_jwt", "unstamped", "no_target", "unknown_member", "private_agent"} {
		test.Run(refusal, func(test *testing.T) {
			fixture := newIntentFixture(test)
			source, taskID, content, want := "task_token", fixture.taskID, fixture.mention(), http.StatusForbidden
			switch refusal {
			case "chat":
				sessionID := createHandlerTestChatSession(test, fixture.agentID)
				dbfx.Exec(test, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, sessionID, taskID)
			case "other_issue":
				dbfx.Exec(test, `UPDATE agent_task_queue SET issue_id = $1 WHERE id = $2`, dbfx.Issue(test, "other issue"), taskID)
			case "other_agent":
				taskID = dbfx.Task(test, createHandlerTestAgent(test, "other agent", []byte("[]")), testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
			case "non_assignee":
				dbfx.Exec(test, `UPDATE issue SET assignee_id = $1 WHERE id = $2`, uuid.NewString(), fixture.issueID)
			case "stopped":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID)
			case "terminal":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
			case "no_target":
				content, want = "no human named", http.StatusBadRequest
			case "unknown_member":
				content, want = "[@Missing](mention://member/"+uuid.NewString()+")", http.StatusBadRequest
			case "private_agent":
				dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, fixture.agentID)
				want = http.StatusBadRequest
			case "unstamped":
				source = ""
			default:
				source = refusal
			}
			fixture.post(test, source, taskID, content, nil).Want(want)
			for _, table := range []string{"comment", "decision_intent"} {
				if count := dbfx.Count(test, "SELECT count(*) FROM "+table+" WHERE issue_id = $1", fixture.issueID); count != 0 {
					test.Fatalf("%s rows = %d after refusal", table, count)
				}
			}
		})
	}
}

func TestDecisionIntentSupersedesAndKeepsTargets(test *testing.T) {
	fixture := newIntentFixture(test)
	secondHuman := dbfx.User(test, "Second human", "intent-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": secondHuman, "role": "member"})
	content := fixture.mention() + " " + fixture.mention() + " [@Second](mention://member/" + secondHuman + ")"
	var first CommentResponse
	fixture.post(test, "task_token", fixture.taskID, content, nil).Want(http.StatusCreated).JSON(&first)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent_target target JOIN decision_intent intent ON intent.id = target.decision_intent_id WHERE intent.comment_id = $1`, first.ID); count != 2 {
		test.Fatalf("deduplicated group targets = %d, want 2", count)
	}
	request := withURLParam(newRequest(http.MethodPut, "/api/comments/"+first.ID, map[string]string{"content": "edited away mentions"}), "commentId", first.ID)
	request.Header.Set("X-Agent-ID", fixture.agentID)
	testutil.Call(test, testHandler.UpdateComment, request).Want(http.StatusOK)
	fixture.post(test, "task_token", fixture.taskID, fixture.mention(), map[string]any{"parent_id": first.ID}).Want(http.StatusCreated)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE comment_id = $1 AND closed_at IS NOT NULL AND sealed_at IS NOT NULL AND close_reason = 'superseded'`, first.ID); count != 1 {
		test.Fatal("previous window not closed and sealed")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent_target target JOIN decision_intent intent ON intent.id = target.decision_intent_id WHERE intent.comment_id = $1`, first.ID); count != 2 {
		test.Fatal("editing or supersession changed fixed group targets")
	}
}

func TestDecisionIntentPublishesNotificationSuppression(test *testing.T) {
	fixture := newIntentFixture(test)
	originalBus := testHandler.Bus
	testHandler.Bus = events.New()
	test.Cleanup(func() { testHandler.Bus = originalBus })
	var targets []string
	testHandler.Bus.Subscribe(protocol.EventCommentCreated, func(event events.Event) {
		targets, _ = event.Payload.(map[string]any)["decision_intent_targets"].([]string)
	})
	fixture.post(test, "task_token", fixture.taskID, fixture.mention(), nil).Want(http.StatusCreated)
	if len(targets) != 1 || targets[0] != fixture.humanID {
		test.Fatalf("suppressed targets = %v", targets)
	}
}

func TestDecisionIntentSealSurvivesRefusal(test *testing.T) {
	for _, reason := range []string{"expired", "stopped", "tombstone", "deleted", "consumed", "sealed"} {
		test.Run(reason, func(test *testing.T) {
			fixture := newIntentFixture(test)
			commentID := dbfx.Comment(test, fixture.issueID, fixture.mention(), testutil.Cols{"author_type": "agent", "author_id": fixture.agentID})
			intentID := dbfx.Insert(test, "decision_intent", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "task_id": fixture.taskID, "agent_id": fixture.agentID, "comment_id": commentID, "expires_at": time.Now().Add(3 * time.Minute)})
			dbfx.Exec(test, `INSERT INTO decision_intent_target (decision_intent_id, human_user_id) VALUES ($1, $2)`, intentID, fixture.humanID)
			switch reason {
			case "expired":
				dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, intentID)
			case "stopped":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
			case "tombstone":
				dbfx.Exec(test, `UPDATE comment SET deleted_at = clock_timestamp(), content = '' WHERE id = $1`, commentID)
			case "deleted":
				dbfx.Exec(test, `DELETE FROM comment WHERE id = $1`, commentID)
			case "consumed":
				dbfx.Exec(test, `UPDATE decision_intent_target SET consumed_at = clock_timestamp() WHERE decision_intent_id = $1`, intentID)
			case "sealed":
				dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, intentID)
			}
			tx := fixture.beginIntentCheck(test)
			intent, err := testHandler.Queries.WithTx(tx).LockDecisionIntent(context.Background(), db.LockDecisionIntentParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(intentID)})
			if err != nil {
				test.Fatal(err)
			}
			if err := decisiontx.ValidateIntent(context.Background(), tx, intent); err == nil {
				test.Fatal("invalid window was accepted")
			}
			_ = tx.Rollback(context.Background())
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, intentID); count != 1 {
				test.Fatal("seal rolled back with refusal")
			}
			if reason == "tombstone" || reason == "deleted" {
				if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND close_reason = 'comment_deleted' AND closed_at IS NOT NULL`, intentID); count != 1 {
					test.Fatal("removed tag did not close window")
				}
			}
		})
	}
}

func (fixture intentFixture) beginIntentCheck(test *testing.T) pgx.Tx {
	test.Helper()
	ctx := context.Background()
	tx, err := decisiontx.Begin(ctx, testPool, parseUUID(testWorkspaceID))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = tx.Rollback(ctx) })
	queries := testHandler.Queries.WithTx(tx)
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(fixture.issueID)}); err != nil {
		test.Fatal(err)
	}
	if _, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(fixture.taskID)}); err != nil {
		test.Fatal(err)
	}
	return tx
}

func TestDecisionIntentWallClockNegativeControl(test *testing.T) {
	fixture := newIntentFixture(test)
	commentID := dbfx.Comment(test, fixture.issueID, fixture.mention(), testutil.Cols{"author_type": "agent", "author_id": fixture.agentID})
	intentID := dbfx.Insert(test, "decision_intent", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "task_id": fixture.taskID, "agent_id": fixture.agentID, "comment_id": commentID, "expires_at": time.Now().Add(3 * time.Minute)})
	dbfx.Exec(test, `INSERT INTO decision_intent_target (decision_intent_id, human_user_id) VALUES ($1, $2)`, intentID, fixture.humanID)
	tx := fixture.beginIntentCheck(test)
	holder, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(context.Background(), `UPDATE decision_intent SET expires_at = clock_timestamp() + interval '200 milliseconds' WHERE id = $1`, intentID); err != nil {
		test.Fatal(err)
	}
	locked := make(chan db.DecisionIntent, 1)
	lockError := make(chan error, 1)
	go func() {
		intent, err := testHandler.Queries.WithTx(tx).LockDecisionIntent(context.Background(), db.LockDecisionIntentParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(intentID)})
		if err != nil {
			lockError <- err
			return
		}
		locked <- intent
	}()
	time.Sleep(300 * time.Millisecond)
	if err := holder.Commit(context.Background()); err != nil {
		test.Fatal(err)
	}
	var intent db.DecisionIntent
	select {
	case intent = <-locked:
	case err := <-lockError:
		test.Fatal(err)
	case <-time.After(3 * time.Second):
		test.Fatal("intent lock did not finish")
	}
	var staleNowAllows, wallClockRefuses, sealed bool
	if err := tx.QueryRow(context.Background(), `SELECT now() < expires_at, clock_timestamp() >= expires_at, sealed_at IS NOT NULL FROM decision_intent WHERE id = $1`, intentID).Scan(&staleNowAllows, &wallClockRefuses, &sealed); err != nil {
		test.Fatal(err)
	}
	if !staleNowAllows || !wallClockRefuses || sealed {
		test.Fatalf("negative control invalid: stale=%t wall=%t sealed=%t", staleNowAllows, wallClockRefuses, sealed)
	}
	if err := decisiontx.ValidateIntent(context.Background(), tx, intent); err == nil {
		test.Fatal("transaction-start clock admitted an expired window")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, intentID); count != 1 {
		test.Fatal("wall-clock refusal did not persist seal")
	}
}

func TestDecisionIntentMiddlewareAuthCannotBeForged(test *testing.T) {
	fixture := newIntentFixture(test)
	credentials := provenanceCredentials(test, fixture.agentID)
	taskToken := "mat_intent_" + fixture.taskID
	dbfx.Insert(test, "task_token", testutil.Cols{
		"token_hash": auth.HashToken(taskToken), "task_id": fixture.taskID, "agent_id": fixture.agentID,
		"workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour),
	})
	credentials["task_token"] = taskToken
	protected := middleware.Auth(testHandler.Queries, nil, provenanceCloudVerifier(test), nil)(http.HandlerFunc(testHandler.CreateComment))
	for source, token := range credentials {
		test.Run(source, func(test *testing.T) {
			before := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID)
			request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+fixture.issueID+"/comments", map[string]any{"content": fixture.mention(), "decision_intent": true}), "id", fixture.issueID)
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", fixture.taskID)
			want := http.StatusForbidden
			if source == "task_token" {
				want = http.StatusCreated
				before++
			}
			testutil.Call(test, protected.ServeHTTP, request).Want(want)
			if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID); count != before {
				test.Fatalf("unexpected comments after %s: %d, want %d", source, count, before)
			}
		})
	}
}

func TestDecisionIntentAtomicRollbackAndRenewal(test *testing.T) {
	fixture := newIntentFixture(test)
	var first CommentResponse
	fixture.post(test, "task_token", fixture.taskID, fixture.mention(), nil).Want(http.StatusCreated).JSON(&first)
	fixture.post(test, "task_token", fixture.taskID, fixture.mention(), map[string]any{"attachment_ids": []string{uuid.NewString()}}).Want(http.StatusConflict)
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID); count != 1 {
		test.Fatal("failed attachment left a comment")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE issue_id = $1 AND closed_at IS NULL AND sealed_at IS NULL`, fixture.issueID); count != 1 {
		test.Fatal("failed attachment superseded the old window or left a new one")
	}
	tx := fixture.beginIntentCheck(test)
	intent, err := testHandler.Queries.WithTx(tx).LockDecisionPendingIntent(context.Background(), db.LockDecisionPendingIntentParams{WorkspaceID: parseUUID(testWorkspaceID), TaskID: parseUUID(fixture.taskID)})
	if err != nil {
		test.Fatal(err)
	}
	if err := decisiontx.ValidateIntent(context.Background(), tx, intent); err != nil {
		test.Fatalf("valid window refused: %v", err)
	}
	if _, err := tx.Exec(context.Background(), "SELECT 1"); err != nil {
		test.Fatalf("valid guard ended transaction: %v", err)
	}
	_ = tx.Rollback(context.Background())
	dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() - interval '1 second', sealed_at = clock_timestamp() WHERE id = $1`, intent.ID)
	fixture.post(test, "task_token", fixture.taskID, fixture.mention(), nil).Want(http.StatusCreated)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE issue_id = $1 AND closed_at IS NULL AND sealed_at IS NULL AND expires_at > clock_timestamp()`, fixture.issueID); count != 1 {
		test.Fatal("new tag did not open a fresh window after expiry")
	}
}

func TestDecisionIntentTaskLockIsNowait(test *testing.T) {
	fixture := newIntentFixture(test)
	ctx := context.Background()
	holder, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT id FROM agent_task_queue WHERE id = $1 FOR UPDATE`, fixture.taskID); err != nil {
		test.Fatal(err)
	}
	originalStarter := testHandler.TxStarter
	starter := &intentRetryStarter{base: originalStarter}
	testHandler.TxStarter = starter
	test.Cleanup(func() { testHandler.TxStarter = originalStarter })
	fixture.post(test, "task_token", fixture.taskID, fixture.mention(), nil).Want(http.StatusServiceUnavailable)
	if starter.attempts != 3 {
		test.Fatalf("transaction attempts = %d, want 3", starter.attempts)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID); count != 0 {
		test.Fatal("busy task created a comment")
	}
}
