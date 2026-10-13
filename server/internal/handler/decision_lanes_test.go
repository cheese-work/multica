package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func openLaneIntent(test *testing.T, fixture intentFixture, content string) (string, string) {
	test.Helper()
	var comment CommentResponse
	fixture.post(test, "task_token", fixture.taskID, content, nil).Want(http.StatusCreated).JSON(&comment)
	var intentID string
	dbfx.QueryRow(test, `SELECT decision_intent_id FROM comment WHERE id = $1`, comment.ID).Scan(&intentID)
	return intentID, comment.ID
}

func laneRequest(fixture intentFixture, target string) *http.Request {
	request := newRequest(http.MethodGet, "/api/chat/sessions?issue="+fixture.issueID+target, nil)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return request
}

func TestDecisionLanesScopeAndNoTranscripts(test *testing.T) {
	fixture := newIntentFixture(test)
	intentID, _ := openLaneIntent(test, fixture, fixture.mention())
	older := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "older", "updated_at": time.Now().Add(-time.Hour)})
	newer := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "newer", "lane_summary": "Release decisions", "lane_summary_updated_at": time.Now()})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": newer, "role": "user", "content": "PRIVATE TRANSCRIPT"})
	for _, excluded := range []string{"creator", "agent", "workspace", "archive", "channel", "channel_origin", "intro"} {
		cols := testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": excluded}
		switch excluded {
		case "creator":
			cols["creator_id"] = testUserID
		case "agent":
			cols["agent_id"] = createHandlerTestAgent(test, "other lane agent", []byte("[]"))
		case "workspace":
			cols["workspace_id"] = dbfx.Workspace(test, "other lane workspace", "lanes-"+uuid.NewString())
		case "archive":
			cols["status"] = "archived"
		case "channel_origin":
			cols["channel_origin_at"] = time.Now()
		case "intro":
			cols["is_agent_intro"] = true
		}
		sessionID := dbfx.Insert(test, "chat_session", cols)
		if excluded == "channel" {
			installationID := dbfx.Insert(test, "channel_installation", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "channel_type": "slack", "installer_user_id": testUserID})
			dbfx.Insert(test, "channel_chat_session_binding", testutil.Cols{"chat_session_id": sessionID, "installation_id": installationID, "channel_type": "slack", "channel_chat_id": "lane-" + sessionID, "chat_type": "p2p"})
		}
	}
	for _, target := range []string{"", "&for=" + fixture.humanID, "&status=all"} {
		var lanes []map[string]any
		response := testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, target)).Want(http.StatusOK)
		response.JSON(&lanes)
		if len(lanes) != 2 || lanes[0]["id"] != newer || lanes[1]["id"] != older || lanes[0]["lane_summary"] != "Release decisions" {
			test.Fatalf("wrong lanes: %+v", lanes)
		}
		for _, lane := range lanes {
			if len(lane) != 5 {
				test.Fatalf("lane must expose only five metadata fields: %+v", lane)
			}
			for _, key := range []string{"id", "title", "lane_summary", "lane_summary_updated_at", "updated_at"} {
				if _, exists := lane[key]; !exists {
					test.Fatalf("missing metadata field %s: %+v", key, lane)
				}
			}
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1 AND consumed_at IS NULL`, intentID); count != 1 {
		test.Fatalf("listings consumed a target: %d", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND consumed_at IS NULL AND sealed_at IS NULL AND closed_at IS NULL`, intentID); count != 1 {
		test.Fatal("listing changed the window")
	}
	request := laneRequest(fixture, "")
	request.URL.RawQuery = ""
	testutil.Call(test, testHandler.ListChatSessions, request).Want(http.StatusForbidden)
	for _, read := range []http.HandlerFunc{testHandler.GetChatSession, testHandler.ListChatMessages, testHandler.ListChatMessagesPage} {
		request := withURLParam(laneRequest(fixture, ""), "sessionId", newer)
		testutil.Call(test, read, request).Want(http.StatusForbidden)
	}
}

func TestDecisionLanesRefusals(test *testing.T) {
	for _, refusal := range []string{"missing", "expired", "closed", "sealed", "consumed", "target_consumed", "tag_tombstone", "tag_delete", "other_run", "other_agent", "other_issue", "stopped", "chat", "non_assignee", "terminal", "missing_member", "private_agent", "archived_agent", "unnamed_target", "missing_task", "invalid_task", "missing_agent", "invalid_agent", "invalid_target", "pat", "cloud_pat", "cli_jwt", "interactive_jwt", "unstamped"} {
		test.Run(refusal, func(test *testing.T) {
			fixture := newIntentFixture(test)
			intentID, commentID := openLaneIntent(test, fixture, fixture.mention())
			request := laneRequest(fixture, "")
			switch refusal {
			case "missing":
				dbfx.Exec(test, `DELETE FROM decision_intent WHERE id = $1`, intentID)
			case "expired":
				dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, intentID)
			case "closed":
				dbfx.Exec(test, `UPDATE decision_intent SET closed_at = now() WHERE id = $1`, intentID)
			case "sealed":
				dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = now() WHERE id = $1`, intentID)
			case "consumed":
				dbfx.Exec(test, `UPDATE decision_intent SET consumed_at = now() WHERE id = $1`, intentID)
			case "target_consumed":
				dbfx.Exec(test, `UPDATE decision_intent_target SET consumed_at = now() WHERE decision_intent_id = $1`, intentID)
			case "tag_tombstone":
				dbfx.Exec(test, `UPDATE comment SET deleted_at = now() WHERE id = $1`, commentID)
			case "tag_delete":
				dbfx.Exec(test, `DELETE FROM comment WHERE id = $1`, commentID)
			case "other_run":
				request.Header.Set("X-Task-ID", dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)}))
			case "other_agent":
				request.Header.Set("X-Agent-ID", createHandlerTestAgent(test, "wrong caller", []byte("[]")))
			case "other_issue":
				dbfx.Exec(test, `UPDATE agent_task_queue SET issue_id = $1 WHERE id = $2`, dbfx.Issue(test, "wrong issue"), fixture.taskID)
			case "stopped":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
			case "chat":
				dbfx.Exec(test, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, createHandlerTestChatSession(test, fixture.agentID), fixture.taskID)
			case "non_assignee":
				dbfx.Exec(test, `UPDATE issue SET assignee_id = NULL, assignee_type = NULL WHERE id = $1`, fixture.issueID)
			case "terminal":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
			case "missing_member":
				dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, fixture.humanID)
			case "private_agent":
				dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, fixture.agentID)
			case "archived_agent":
				dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, fixture.agentID)
			case "unnamed_target":
				request.URL.RawQuery += "&for=" + testUserID
			case "invalid_target":
				request.URL.RawQuery += "&for=not-a-user-id"
			case "missing_task":
				request.Header.Del("X-Task-ID")
			case "invalid_task":
				request.Header.Set("X-Task-ID", "not-a-task-id")
			case "missing_agent":
				request.Header.Del("X-Agent-ID")
			case "invalid_agent":
				request.Header.Set("X-Agent-ID", "not-an-agent-id")
			default:
				request.Header.Set("X-Actor-Source", map[string]string{"unstamped": ""}[refusal])
				if refusal != "unstamped" {
					request.Header.Set("X-Actor-Source", refusal)
				}
			}
			testutil.Call(test, testHandler.ListChatSessions, request).Want(http.StatusForbidden)
			if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1 AND creator_id = $2`, fixture.agentID, fixture.humanID); count != 0 {
				test.Fatalf("refusal created %d default lanes", count)
			}
		})
	}
}

func TestDecisionLanesSquadAndAuthProvenance(test *testing.T) {
	fixture := newIntentFixture(test)
	squadID := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Lane squad", "creator_id": testUserID, "leader_id": fixture.agentID})
	dbfx.Exec(test, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, squadID, fixture.agentID)
	dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
	openLaneIntent(test, fixture, fixture.mention())
	credentials := provenanceCredentials(test, fixture.agentID)
	boundToken := "mat_bound_lanes_" + fixture.taskID
	dbfx.Insert(test, "task_token", testutil.Cols{"token_hash": auth.HashToken(boundToken), "task_id": fixture.taskID, "agent_id": fixture.agentID, "workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour)})
	credentials["bound_task"] = boundToken
	protected := middleware.Auth(testHandler.Queries, nil, provenanceCloudVerifier(test), nil)(http.HandlerFunc(testHandler.ListChatSessions))
	for caller, token := range credentials {
		test.Run(caller, func(test *testing.T) {
			request := laneRequest(fixture, "&for="+fixture.humanID)
			request.Header.Set("Authorization", "Bearer "+token)
			want := http.StatusForbidden
			if caller == "bound_task" {
				want = http.StatusOK
			}
			testutil.Call(test, protected.ServeHTTP, request).Want(want)
		})
	}
	dbfx.Exec(test, `DELETE FROM squad_member WHERE squad_id = $1 AND member_id = $2`, squadID, fixture.agentID)
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "")).Want(http.StatusForbidden)
}

type laneFenceStarter struct {
	base                         txStarter
	intentID                     string
	staleNowAllows, clockRefuses bool
}

type laneTargetStarter struct {
	base    txStarter
	fences  int
	consume func()
}

func (starter *laneTargetStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.base.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return laneTargetTransaction{Tx: transaction, starter: starter}, nil
}

type laneTargetTransaction struct {
	pgx.Tx
	starter *laneTargetStarter
}

func (transaction laneTargetTransaction) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "DecisionLaneWindowOpen") {
		transaction.starter.fences++
		if transaction.starter.fences == 2 {
			transaction.starter.consume()
		}
	}
	return transaction.Tx.QueryRow(ctx, query, args...)
}

func TestDecisionLanesTargetConsumptionFence(test *testing.T) {
	fixture := newIntentFixture(test)
	intentID, _ := openLaneIntent(test, fixture, fixture.mention()+" [@Owner](mention://member/"+testUserID+")")
	originalStarter := testHandler.TxStarter
	starter := &laneTargetStarter{base: originalStarter, consume: func() {
		dbfx.Exec(test, `UPDATE decision_intent_target SET consumed_at = clock_timestamp() WHERE decision_intent_id = $1 AND human_user_id = $2`, intentID, fixture.humanID)
	}}
	testHandler.TxStarter = starter
	test.Cleanup(func() { testHandler.TxStarter = originalStarter })
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "&for="+fixture.humanID)).Want(http.StatusForbidden)
	if starter.fences != 2 || dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, fixture.agentID) != 0 {
		test.Fatalf("consumption during final fence did not roll back creation: fences=%d", starter.fences)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1 AND consumed_at IS NULL`, intentID); count != 1 {
		test.Fatalf("wrong remaining group size: %d", count)
	}
}

func (starter *laneFenceStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.base.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return laneFenceTransaction{Tx: transaction, starter: starter}, nil
}

type laneFenceTransaction struct {
	pgx.Tx
	starter *laneFenceStarter
}

func (transaction laneFenceTransaction) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	row := transaction.Tx.QueryRow(ctx, query, args...)
	if strings.Contains(query, "LockWorkspaceForChatSessionCreate") {
		return laneFenceRow{Row: row, transaction: transaction, ctx: ctx}
	}
	return row
}

type laneFenceRow struct {
	pgx.Row
	transaction laneFenceTransaction
	ctx         context.Context
}

func (row laneFenceRow) Scan(values ...any) error {
	if err := row.Row.Scan(values...); err != nil {
		return err
	}
	starter := row.transaction.starter
	return row.transaction.Tx.QueryRow(row.ctx, `SELECT now() < expires_at, clock_timestamp() >= expires_at FROM decision_intent WHERE id = $1`, starter.intentID).Scan(&starter.staleNowAllows, &starter.clockRefuses)
}

func TestDecisionLanesGroupTargets(test *testing.T) {
	fixture := newIntentFixture(test)
	intentID, commentID := openLaneIntent(test, fixture, fixture.mention()+" [@Owner](mention://member/"+testUserID+")")
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "")).Want(http.StatusForbidden)
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "&for="+fixture.humanID)).Want(http.StatusOK)
	dbfx.Exec(test, `UPDATE decision_intent_target SET consumed_at = now() WHERE decision_intent_id = $1 AND human_user_id = $2`, intentID, fixture.humanID)
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "&for="+fixture.humanID)).Want(http.StatusForbidden)
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "")).Want(http.StatusForbidden)
	dbfx.Exec(test, `UPDATE comment SET content = $1 WHERE id = $2`, fixture.mention(), commentID)
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "&for="+testUserID)).Want(http.StatusOK)
}

func TestDecisionLanesDefaultHasNoDecisionLocks(test *testing.T) {
	fixture := newIntentFixture(test)
	intentID, _ := openLaneIntent(test, fixture, fixture.mention())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	holder, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(ctx, `SELECT id FROM issue WHERE id = $1 FOR UPDATE`, fixture.issueID); err != nil {
		test.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT id FROM decision_intent WHERE id = $1 FOR UPDATE`, intentID); err != nil {
		test.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT human_user_id FROM decision_intent_target WHERE decision_intent_id = $1 FOR UPDATE`, intentID); err != nil {
		test.Fatal(err)
	}
	var lanes []map[string]any
	request := laneRequest(fixture, "").WithContext(ctx)
	testutil.Call(test, testHandler.ListChatSessions, request).Want(http.StatusOK).JSON(&lanes)
	if len(lanes) != 1 || lanes[0]["title"] != "Decisions: decision-intent" {
		test.Fatalf("wrong default lane: %+v", lanes)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE id = $1 AND creator_id = $2 AND explicitly_created_at IS NOT NULL AND channel_origin_at IS NULL`, lanes[0]["id"], fixture.humanID); count != 1 {
		test.Fatal("default lane is not an explicitly created web DM for the named person")
	}
}

func TestDecisionLanesFenceAfterWorkspaceWait(test *testing.T) {
	fixture := newIntentFixture(test)
	intentID, _ := openLaneIntent(test, fixture, fixture.mention())
	originalStarter := testHandler.TxStarter
	starter := &laneFenceStarter{base: originalStarter, intentID: intentID}
	testHandler.TxStarter = starter
	test.Cleanup(func() { testHandler.TxStarter = originalStarter })
	dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() + interval '500 milliseconds' WHERE id = $1`, intentID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	holder, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(ctx, `SELECT id FROM workspace WHERE id = $1 FOR UPDATE`, testWorkspaceID); err != nil {
		test.Fatal(err)
	}
	writer := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		testHandler.ListChatSessions(writer, laneRequest(fixture, "").WithContext(ctx))
	}()
	blocked := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		var count int
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FOR KEY SHARE%'`).Scan(&count); err != nil {
			test.Fatal(err)
		}
		if count > 0 {
			blocked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_sleep(0.6)`); err != nil {
		test.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	<-done
	if !blocked || !starter.staleNowAllows || !starter.clockRefuses || writer.Code != http.StatusForbidden {
		test.Fatalf("post-wait fence: blocked=%t stale-now-allows=%t clock-refuses=%t status=%d body=%s", blocked, starter.staleNowAllows, starter.clockRefuses, writer.Code, writer.Body)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, fixture.agentID); count != 0 {
		test.Fatalf("expired window created %d lanes", count)
	}
}

func TestDecisionLanesDeleteAfterListing(test *testing.T) {
	for _, physical := range []bool{false, true} {
		test.Run(fmt.Sprintf("physical=%t", physical), func(test *testing.T) {
			fixture := newIntentFixture(test)
			_, commentID := openLaneIntent(test, fixture, fixture.mention())
			writer := httptest.NewRecorder()
			testHandler.ListChatSessions(writer, laneRequest(fixture, ""))
			if writer.Code != http.StatusOK {
				test.Fatalf("first listing: %d %s", writer.Code, writer.Body)
			}
			var lanes []map[string]any
			if err := json.Unmarshal(writer.Body.Bytes(), &lanes); err != nil {
				test.Fatal(err)
			}
			if physical {
				dbfx.Exec(test, `DELETE FROM comment WHERE id = $1`, commentID)
			} else {
				dbfx.Exec(test, `UPDATE comment SET deleted_at = now() WHERE id = $1`, commentID)
			}
			testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "")).Want(http.StatusForbidden)
			if len(lanes) != 1 || strings.Contains(writer.Body.String(), "messages") {
				test.Fatalf("metadata-only listing: %s", writer.Body)
			}
		})
	}
}
