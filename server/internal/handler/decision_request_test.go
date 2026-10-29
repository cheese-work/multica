package handler

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type requestCoreFixture struct {
	intentFixture
	issue                      db.Issue
	intentID, tagID, sessionID string
}

func newRequestCoreFixture(test *testing.T, group bool) requestCoreFixture {
	test.Helper()
	fixture := newIntentFixture(test)
	content := fixture.mention()
	if group {
		content += " [@Owner](mention://member/" + testUserID + ")"
	}
	intentID, tagID := openLaneIntent(test, fixture, content)
	sessionID := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "Decision lane"})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "see [issue](mention://issue/" + fixture.issueID + ")"})
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM decision_request WHERE issue_id = $1`, fixture.issueID); err != nil {
			test.Error(err)
		}
	})
	return requestCoreFixture{intentFixture: fixture, issue: issue, intentID: intentID, tagID: tagID, sessionID: sessionID}
}

func (fixture requestCoreFixture) input() decisionRequestInput {
	return decisionRequestInput{SessionID: parseUUID(fixture.sessionID), Ask: "Approve the reversible change?", EscalationClass: "reversible"}
}

func (fixture requestCoreFixture) request() *http.Request {
	return laneRequest(fixture.intentFixture, "")
}

func (fixture requestCoreFixture) assertCounts(test *testing.T, wanted int) {
	test.Helper()
	for label, query := range map[string]string{
		"requests": `SELECT count(*) FROM decision_request WHERE issue_id = $1`,
		"asks":     `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'ask'`,
		"pings":    `SELECT count(*) FROM chat_message message JOIN decision_request request ON request.ping_message_id = message.id WHERE request.issue_id = $1`,
	} {
		if count := dbfx.Count(test, query, fixture.issueID); count != wanted {
			test.Fatalf("%s = %d, want %d", label, count, wanted)
		}
	}
	if wanted == 0 && dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID) != 0 {
		test.Fatal("refusal left a DM message")
	}
}

func requireRequestRefusal(test *testing.T, err error) {
	test.Helper()
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || (refusal.status != http.StatusForbidden && refusal.status != http.StatusConflict && refusal.status != http.StatusBadRequest) {
		test.Fatalf("expected explicit refusal, got %v", err)
	}
}

func TestDecisionRequestCoreRoundTrip(test *testing.T) {
	for _, squad := range []bool{false, true} {
		test.Run(map[bool]string{false: "agent", true: "squad"}[squad], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			if squad {
				squadID := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Request squad", "creator_id": testUserID, "leader_id": fixture.agentID})
				dbfx.Exec(test, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, squadID, fixture.agentID)
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
			}
			dbfx.Exec(test, `UPDATE comment SET resolved_at = now(), resolved_by_type = 'member', resolved_by_id = $1 WHERE id = $2`, testUserID, fixture.tagID)
			input := fixture.input()
			input.Ask = "  Keep the human's exact ask.\nSecond line.  "
			result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
			if err != nil {
				test.Fatal(err)
			}
			stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: result.Request.ID})
			if err != nil {
				test.Fatal(err)
			}
			if !reflect.DeepEqual(stored, result.Request) || stored.IssueID != fixture.issue.ID || stored.AskingAgentID != parseUUID(fixture.agentID) || stored.HumanUserID != parseUUID(fixture.humanID) || stored.ChatSessionID != input.SessionID || stored.DecisionIntentID != parseUUID(fixture.intentID) || stored.Ask != input.Ask || stored.Origin != "asked" || stored.Status != "open" || stored.EscalationClass != input.EscalationClass || stored.PingNotifiedAt.Valid {
				test.Fatalf("wrong request: %+v", stored)
			}
			if result.Comment.ID != stored.IssueCommentID || result.Comment.ParentID != parseUUID(fixture.tagID) || result.Comment.DecisionRequestID != stored.ID || result.Comment.DecisionCommentKind.String != "ask" || result.Comment.AuthorType != "agent" || result.Comment.AuthorID != stored.AskingAgentID || result.Comment.SourceTaskID != parseUUID(fixture.taskID) || result.Comment.Content != input.Ask {
				test.Fatalf("wrong ask comment: %+v", result.Comment)
			}
			if result.Message.ID != stored.PingMessageID || result.Message.ChatSessionID != stored.ChatSessionID || result.Message.MessageKind != "decision_request" || result.Message.Role != "assistant" || result.Message.SentVia.String != "task_token" || result.Message.TaskID.Valid || result.Message.Content != input.Ask {
				test.Fatalf("wrong non-turn ping: %+v", result.Message)
			}
			fixture.assertCounts(test, 1)
			if len(result.ClearedComments) != 1 || result.ClearedComments[0].ID != parseUUID(fixture.tagID) || result.ClearedComments[0].ResolvedAt.Valid || dbfx.Count(test, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, fixture.tagID) != 0 {
				test.Fatal("committed ask did not return its cleared thread resolution")
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND consumed_at IS NOT NULL AND sealed_at IS NOT NULL AND consumed_by_request_id = $2`, fixture.intentID, uuidToString(stored.ID)) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1 AND consumed_at IS NOT NULL AND consumed_by_request_id = $2`, fixture.intentID, uuidToString(stored.ID)) != 1 {
				test.Fatal("request did not atomically consume and seal its target")
			}
			if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, fixture.sessionID) != 0 || dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID) != 0 {
				test.Fatal("core started a turn or inserted an inbox item")
			}
		})
	}
}

func TestDecisionRequestCoreRefusals(test *testing.T) {
	for _, reason := range []string{"pat", "cloud_pat", "cli_jwt", "interactive_jwt", "unstamped", "missing_task", "invalid_task", "missing_agent", "invalid_agent", "other_task", "other_agent", "chat_task", "other_issue", "non_assignee", "archived_agent", "private_agent", "missing_member", "untagged", "other_session_agent", "other_workspace", "archived_lane", "channel_origin", "channel_binding", "intro_lane", "missing_lane", "closed_intent", "sealed_intent", "missing_intent", "wrong_tag_author", "wrong_tag_task", "empty_ask", "invalid_class", "terminal"} {
		test.Run(reason, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			request, input := fixture.request(), fixture.input()
			switch reason {
			case "pat", "cloud_pat", "cli_jwt", "interactive_jwt":
				request.Header.Set("X-Actor-Source", reason)
			case "unstamped":
				request.Header.Del("X-Actor-Source")
			case "missing_task":
				request.Header.Del("X-Task-ID")
			case "invalid_task":
				request.Header.Set("X-Task-ID", "bad-task")
			case "missing_agent":
				request.Header.Del("X-Agent-ID")
			case "invalid_agent":
				request.Header.Set("X-Agent-ID", "bad-agent")
			case "other_task":
				request.Header.Set("X-Task-ID", dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)}))
			case "other_agent":
				request.Header.Set("X-Agent-ID", createHandlerTestAgent(test, "wrong requester", []byte("[]")))
			case "chat_task":
				dbfx.Exec(test, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, fixture.sessionID, fixture.taskID)
			case "other_issue":
				dbfx.Exec(test, `UPDATE agent_task_queue SET issue_id = $1 WHERE id = $2`, dbfx.Issue(test, "wrong issue"), fixture.taskID)
			case "non_assignee":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = NULL, assignee_id = NULL WHERE id = $1`, fixture.issueID)
			case "archived_agent":
				dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, fixture.agentID)
			case "private_agent":
				dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, fixture.agentID)
			case "missing_member":
				dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, fixture.humanID)
			case "untagged":
				dbfx.Exec(test, `UPDATE chat_session SET creator_id = $1 WHERE id = $2`, testUserID, fixture.sessionID)
			case "other_session_agent":
				dbfx.Exec(test, `UPDATE chat_session SET agent_id = $1 WHERE id = $2`, createHandlerTestAgent(test, "wrong lane agent", []byte("[]")), fixture.sessionID)
			case "other_workspace":
				dbfx.Exec(test, `UPDATE chat_session SET workspace_id = $1 WHERE id = $2`, dbfx.Workspace(test, "wrong lane workspace", "wrong-lane-"+fixture.sessionID), fixture.sessionID)
			case "archived_lane":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			case "channel_origin":
				dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, fixture.sessionID)
			case "channel_binding":
				installationID := dbfx.Insert(test, "channel_installation", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "channel_type": "slack", "installer_user_id": testUserID})
				dbfx.Insert(test, "channel_chat_session_binding", testutil.Cols{"chat_session_id": fixture.sessionID, "installation_id": installationID, "channel_type": "slack", "channel_chat_id": "request-lane-" + fixture.sessionID, "chat_type": "p2p"})
			case "intro_lane":
				dbfx.Exec(test, `UPDATE chat_session SET is_agent_intro = true WHERE id = $1`, fixture.sessionID)
			case "missing_lane":
				input.SessionID = parseUUID("11111111-1111-4111-8111-111111111111")
			case "closed_intent":
				dbfx.Exec(test, `UPDATE decision_intent SET closed_at = now() WHERE id = $1`, fixture.intentID)
			case "sealed_intent":
				dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = now() WHERE id = $1`, fixture.intentID)
			case "missing_intent":
				dbfx.Exec(test, `DELETE FROM decision_intent WHERE id = $1`, fixture.intentID)
			case "wrong_tag_author":
				dbfx.Exec(test, `UPDATE comment SET author_type = 'member', author_id = $1 WHERE id = $2`, testUserID, fixture.tagID)
			case "wrong_tag_task":
				dbfx.Exec(test, `UPDATE comment SET source_task_id = NULL WHERE id = $1`, fixture.tagID)
			case "empty_ask":
				input.Ask = " \n\t"
			case "invalid_class":
				input.EscalationClass = "unknown"
			case "terminal":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
			}
			_, err := testHandler.createDecisionRequest(request, fixture.issue, input)
			requireRequestRefusal(test, err)
			fixture.assertCounts(test, 0)
		})
	}
}

func TestDecisionRequestCoreSpoofedProvenance(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	protected := middleware.Auth(testHandler.Queries, nil, provenanceCloudVerifier(test), nil)(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, err := testHandler.createDecisionRequest(request, fixture.issue, fixture.input())
		var refusal decisionIntentRefusal
		if !errors.As(err, &refusal) {
			writeError(writer, http.StatusInternalServerError, "missing refusal")
			return
		}
		writeError(writer, refusal.status, refusal.message)
	}))
	for source, token := range provenanceCredentials(test, fixture.agentID) {
		test.Run(source, func(test *testing.T) {
			request := fixture.request()
			request.Header.Set("Authorization", "Bearer "+token)
			testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusForbidden)
			fixture.assertCounts(test, 0)
		})
	}
}

func TestDecisionRequestCoreGroupConsumption(test *testing.T) {
	fixture := newRequestCoreFixture(test, true)
	first, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NULL AND consumed_at IS NULL`, fixture.intentID) != 1 {
		test.Fatal("first target sealed an incomplete group")
	}
	_, err = testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	requireRequestRefusal(test, err)
	fixture.assertCounts(test, 1)
	secondSession := laneNamingIssue(test, fixture.issueID, fixture.agentID, testUserID, "Owner lane")
	input := fixture.input()
	input.SessionID = parseUUID(secondSession)
	second, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	fixture.assertCounts(test, 2)
	if second.Request.ID == first.Request.ID || dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL AND consumed_at IS NOT NULL`, fixture.intentID) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1 AND consumed_at IS NOT NULL`, fixture.intentID) != 2 {
		test.Fatal("last target did not complete the group")
	}
}

type requestCoreOutcome struct {
	result decisionRequestResult
	err    error
}

func startRequestCore(test *testing.T, fixture requestCoreFixture, input decisionRequestInput) <-chan requestCoreOutcome {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	test.Cleanup(cancel)
	finished := make(chan requestCoreOutcome, 1)
	go func() {
		result, err := testHandler.createDecisionRequest(fixture.request().WithContext(ctx), fixture.issue, input)
		finished <- requestCoreOutcome{result: result, err: err}
	}()
	return finished
}

func awaitRequestCore(test *testing.T, finished <-chan requestCoreOutcome) requestCoreOutcome {
	test.Helper()
	select {
	case outcome := <-finished:
		return outcome
	case <-time.After(10 * time.Second):
		test.Fatal("request transaction did not finish")
		return requestCoreOutcome{}
	}
}

func TestDecisionRequestCoreConcurrentConsumption(test *testing.T) {
	for _, group := range []bool{false, true} {
		test.Run(map[bool]string{false: "same_target", true: "different_targets"}[group], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, group)
			input := fixture.input()
			if group {
				input.SessionID = parseUUID(laneNamingIssue(test, fixture.issueID, fixture.agentID, testUserID, "Concurrent owner lane"))
			}
			first := startRequestCore(test, fixture, fixture.input())
			second := startRequestCore(test, fixture, input)
			outcomes := []requestCoreOutcome{awaitRequestCore(test, first), awaitRequestCore(test, second)}
			created := 0
			for _, outcome := range outcomes {
				if outcome.err == nil {
					created++
				} else {
					requireRequestRefusal(test, outcome.err)
				}
			}
			wanted := 1
			if group {
				wanted = 2
			}
			if created != wanted {
				test.Fatalf("created %d requests, want %d", created, wanted)
			}
			fixture.assertCounts(test, wanted)
			if dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, fixture.intentID) != 1 {
				test.Fatal("concurrent consumption left the completed group open")
			}
		})
	}
}

type requestCoreStarter struct {
	base     txStarter
	query    string
	before   func(pgx.Tx) error
	once     sync.Once
	fail     error
	attempts int
}

func (starter *requestCoreStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	starter.attempts++
	transaction, err := starter.base.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return requestCoreTransaction{Tx: transaction, starter: starter}, nil
}

type requestCoreTransaction struct {
	pgx.Tx
	starter *requestCoreStarter
}

type requestCoreErrorRow struct{ err error }

func (row requestCoreErrorRow) Scan(_ ...any) error { return row.err }

func (transaction requestCoreTransaction) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	if err := transaction.beforeQuery(query); err != nil {
		return requestCoreErrorRow{err: err}
	}
	return transaction.Tx.QueryRow(ctx, query, arguments...)
}

func (transaction requestCoreTransaction) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	if err := transaction.beforeQuery(query); err != nil {
		return nil, err
	}
	return transaction.Tx.Query(ctx, query, arguments...)
}

func (transaction requestCoreTransaction) beforeQuery(query string) error {
	if strings.Contains(query, transaction.starter.query) {
		if transaction.starter.fail != nil {
			return transaction.starter.fail
		}
		var hookError error
		transaction.starter.once.Do(func() { hookError = transaction.starter.before(transaction.Tx) })
		if hookError != nil {
			return hookError
		}
	}
	return nil
}

func (transaction requestCoreTransaction) Exec(ctx context.Context, query string, arguments ...any) (pgconn.CommandTag, error) {
	if err := transaction.beforeQuery(query); err != nil {
		return pgconn.CommandTag{}, err
	}
	return transaction.Tx.Exec(ctx, query, arguments...)
}

func useRequestCoreStarter(test *testing.T, starter txStarter) {
	test.Helper()
	original := testHandler.TxStarter
	testHandler.TxStarter = starter
	test.Cleanup(func() { testHandler.TxStarter = original })
}

func TestDecisionRequestCoreAtomicRollback(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	dbfx.Exec(test, `UPDATE comment SET resolved_at = now(), resolved_by_type = 'member', resolved_by_id = $1 WHERE id = $2`, testUserID, fixture.tagID)
	starter := &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: CreateChatMessage", fail: &pgconn.PgError{Code: "23514", Message: "injected message insert refusal"}}
	useRequestCoreStarter(test, starter)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err == nil {
		test.Fatal("injected message failure committed")
	}
	if result.Request.ID.Valid || result.Comment.ID.Valid || result.Message.ID.Valid || len(result.ClearedComments) != 0 {
		test.Fatal("failed transaction returned uncommitted objects")
	}
	fixture.assertCounts(test, 0)
	if starter.attempts != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1 AND consumed_at IS NULL`, fixture.intentID) != 1 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE id = $1 AND resolved_at IS NOT NULL`, fixture.tagID) != 1 {
		test.Fatal("rollback retried or lost target/thread state")
	}
}

func TestDecisionRequestCoreTaskCompletionAndBusyRetries(test *testing.T) {
	for _, complete := range []bool{false, true} {
		test.Run(map[bool]string{false: "busy_exhaustion", true: "completion_wins"}[complete], func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			ctx := context.Background()
			holder, err := testPool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer holder.Rollback(ctx)
			if _, err := holder.Exec(ctx, `SELECT id FROM agent_task_queue WHERE id = $1 FOR UPDATE`, fixture.taskID); err != nil {
				test.Fatal(err)
			}
			starter := &intentRetryStarter{base: testHandler.TxStarter}
			if complete {
				starter.onBusy = func() {
					if _, err := holder.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID); err != nil {
						test.Fatal(err)
					}
					if err := holder.Commit(ctx); err != nil {
						test.Fatal(err)
					}
				}
			}
			useRequestCoreStarter(test, starter)
			_, err = testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
			if complete {
				requireRequestRefusal(test, err)
				if starter.attempts != 2 || dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, fixture.intentID) != 1 {
					test.Fatal("completed task did not persist the seal on retry")
				}
			} else if !decisiontx.IsRetryable(err) || starter.attempts != 3 {
				test.Fatalf("busy attempts=%d error=%v", starter.attempts, err)
			}
			fixture.assertCounts(test, 0)
		})
	}
}

func TestDecisionRequestCoreDeadlockRetryBound(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	starter := &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: LockDecisionSession", fail: &pgconn.PgError{Code: "40P01", Message: "named workspace/session exception"}}
	useRequestCoreStarter(test, starter)
	_, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if !decisiontx.IsRetryable(err) || starter.attempts != 2 {
		test.Fatalf("deadlock attempts=%d error=%v", starter.attempts, err)
	}
	fixture.assertCounts(test, 0)
}

func TestDecisionRequestCoreBaselineWins(test *testing.T) {
	for _, mutation := range []string{"issue_close", "tag_delete", "tag_tombstone", "expiry"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			ctx := context.Background()
			holder, err := testPool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer holder.Rollback(ctx)
			var holderPID int
			if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				test.Fatal(err)
			}
			if mutation == "expiry" {
				if _, err := holder.Exec(ctx, `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, fixture.sessionID); err != nil {
					test.Fatal(err)
				}
				dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() + interval '1 second' WHERE id = $1`, fixture.intentID)
			} else {
				if _, err := holder.Exec(ctx, `SELECT id FROM issue WHERE id = $1 FOR NO KEY UPDATE`, fixture.issueID); err != nil {
					test.Fatal(err)
				}
				query := map[string]string{"issue_close": `UPDATE issue SET status = 'done' WHERE id = $1`, "tag_delete": `DELETE FROM comment WHERE id = $1`, "tag_tombstone": `UPDATE comment SET deleted_at = now() WHERE id = $1`}[mutation]
				rowID := fixture.tagID
				if mutation == "issue_close" {
					rowID = fixture.issueID
				}
				if _, err := holder.Exec(ctx, query, rowID); err != nil {
					test.Fatal(err)
				}
			}
			finished := startRequestCore(test, fixture, fixture.input())
			if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
				test.Fatal("ping did not wait behind the baseline fence")
			}
			if mutation == "expiry" {
				time.Sleep(1100 * time.Millisecond)
			}
			if err := holder.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			requireRequestRefusal(test, awaitRequestCore(test, finished).err)
			fixture.assertCounts(test, 0)
			if mutation != "issue_close" && dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, fixture.intentID) != 1 {
				test.Fatal("invalid window seal rolled back with refusal")
			}
			if strings.HasPrefix(mutation, "tag_") && dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND closed_at IS NOT NULL AND close_reason = 'comment_deleted'`, fixture.intentID) != 1 {
				test.Fatal("removed tag did not persist the close reason")
			}
		})
	}
}

func TestDecisionRequestCorePingWins(test *testing.T) {
	for _, mutation := range []string{"issue_close", "tag_tombstone", "task_complete"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newRequestCoreFixture(test, false)
			locked := make(chan int, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			starter := &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: CreateDecisionRequest", before: func(transaction pgx.Tx) error {
				var pid int
				if err := transaction.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return err
				}
				locked <- pid
				select {
				case <-release:
					return nil
				case <-time.After(5 * time.Second):
					return errors.New("ping fence probe timed out")
				}
			}}
			useRequestCoreStarter(test, starter)
			finished := startRequestCore(test, fixture, fixture.input())
			var pingPID int
			select {
			case pingPID = <-locked:
			case <-time.After(3 * time.Second):
				test.Fatal("ping did not reach its guarded insert")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			writerDone := make(chan error, 1)
			go func() {
				transaction, err := testPool.Begin(ctx)
				if err != nil {
					writerDone <- err
					return
				}
				defer transaction.Rollback(ctx)
				if mutation != "task_complete" {
					_, err = transaction.Exec(ctx, `SELECT id FROM issue WHERE id = $1 FOR NO KEY UPDATE`, fixture.issueID)
				}
				if err == nil {
					query := map[string]string{"issue_close": `UPDATE issue SET status = 'done' WHERE id = $1`, "tag_tombstone": `UPDATE comment SET deleted_at = now() WHERE id = $1`, "task_complete": `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`}[mutation]
					rowID := fixture.issueID
					if mutation == "tag_tombstone" {
						rowID = fixture.tagID
					} else if mutation == "task_complete" {
						rowID = fixture.taskID
					}
					_, err = transaction.Exec(ctx, query, rowID)
				}
				if err == nil {
					err = transaction.Commit(ctx)
				}
				writerDone <- err
			}()
			if !waitForWaiterBlockedBy(test, pingPID, 3*time.Second) {
				test.Fatal("baseline writer bypassed the ping fence")
			}
			unblock()
			if outcome := awaitRequestCore(test, finished); outcome.err != nil {
				test.Fatal(outcome.err)
			}
			if err := <-writerDone; err != nil {
				test.Fatal(err)
			}
			fixture.assertCounts(test, 1)
		})
	}
}

func TestDecisionRequestAttachesToIntentCommentWhenAskMatches(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	input := fixture.input()
	input.Ask = fixture.mention()
	before := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	if after := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID); after != before {
		test.Fatalf("comments = %d, want %d (no second ask comment)", after, before)
	}
	fixture.assertCounts(test, 1)
	if result.Comment.ID != parseUUID(fixture.tagID) || result.Request.IssueCommentID != result.Comment.ID || result.Comment.DecisionRequestID != result.Request.ID || result.Comment.DecisionCommentKind.String != "ask" || result.Comment.DecisionIntentID != parseUUID(fixture.intentID) {
		test.Fatalf("ask did not attach to the intent comment: %+v", result.Comment)
	}
}

func TestDecisionRequestMatchingAskKeepsEachTargetsAskComment(test *testing.T) {
	fixture := newRequestCoreFixture(test, true)
	var content string
	dbfx.QueryRow(test, `SELECT content FROM comment WHERE id = $1`, fixture.tagID).Scan(&content)
	input := fixture.input()
	input.Ask = content
	first, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	input.SessionID = parseUUID(laneNamingIssue(test, fixture.issueID, fixture.agentID, testUserID, "Owner lane"))
	second, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	fixture.assertCounts(test, 2)
	for _, result := range []decisionRequestResult{first, second} {
		stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: result.Request.ID})
		if err != nil {
			test.Fatal(err)
		}
		if dbfx.Count(test, `SELECT count(*) FROM comment WHERE id = $1 AND decision_request_id = $2 AND decision_comment_kind = 'ask'`, uuidToString(stored.IssueCommentID), uuidToString(stored.ID)) != 1 {
			test.Fatalf("request %s lost its ask comment link", uuidToString(stored.ID))
		}
	}
}
