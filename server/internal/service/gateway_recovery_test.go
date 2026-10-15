package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGatewayFailureBlocksFreshDispatchUntilCooldown(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	fixture := dbfx.New(pool, workspaceID, userID)
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	failedID := fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "failed",
		"failure_reason": "agent_error.provider_server_error",
		"error":          "unexpected status 503 Service Unavailable", "completed_at": dbfx.Raw("now()"),
	})
	freshIssueID := fixture.Issue(t, "fresh leader dispatch", dbfx.Cols{
		"assignee_type": "agent", "assignee_id": agentID,
	})
	queuedID := fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": freshIssueID, "status": "queued",
	})
	queries := db.New(pool)
	svc := &TaskService{Queries: queries, TxStarter: pool, Bus: events.New()}
	claimed, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil {
		t.Fatalf("gateway cooldown claimed fresh task %s, want nil", util.UUIDToString(claimed.ID))
	}
	queued, err := queries.GetAgentTask(ctx, util.MustParseUUID(queuedID))
	if err != nil || queued.Status != "queued" || queued.FailureReason.Valid {
		t.Fatalf("queued work must survive cooldown: task=%+v err=%v", queued, err)
	}
	fixture.Exec(t, `UPDATE agent_task_queue SET completed_at = now() - interval '11 minutes' WHERE id = $1`, failedID)
	claimed, err = svc.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil || claimed == nil || claimed.ID != util.MustParseUUID(queuedID) {
		t.Fatalf("same queued task must be claimable after cooldown: task=%+v err=%v", claimed, err)
	}
}

func TestResponseProtectionFailTaskStopsRetryAndRejectsSession(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	fixture := dbfx.New(pool, workspaceID, userID)
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "completed",
		"session_id": "poisoned-session", "completed_at": dbfx.Raw("now() - interval '1 minute'"),
	})
	taskID := fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running",
		"session_id": "poisoned-session", "attempt": 1, "max_attempts": 3,
	})
	queries := db.New(pool)
	svc := &TaskService{Queries: queries, TxStarter: pool, Bus: events.New()}
	errText := "stream disconnected before completion: response protection is unavailable"
	failed, err := svc.FailTask(ctx, util.MustParseUUID(taskID), errText, "poisoned-session", "", "", "agent_error.provider_network", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if failed.FailureReason.String != "api_invalid_request" {
		t.Errorf("failure reason = %q, want api_invalid_request", failed.FailureReason.String)
	}
	var retries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, taskID).Scan(&retries); err != nil {
		t.Fatal(err)
	}
	if retries != 0 {
		t.Errorf("response protection failure created %d automatic retries, want 0", retries)
	}
	if !ResumeUnsafeFailure("agent_error.provider_network", errText) {
		t.Error("manual rerun must reject the poisoned session even for legacy rows")
	}
	prior, err := queries.GetLastTaskSession(ctx, db.GetLastTaskSessionParams{
		AgentID: util.MustParseUUID(agentID), IssueID: util.MustParseUUID(issueID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("next task must not resume poisoned session: session=%q err=%v", prior.SessionID.String, err)
	}
	fixture.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id = $1`, taskID)
}

func TestResponseProtectionLegacySessionSelection(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	fixture := dbfx.New(pool, workspaceID, userID)
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "completed", "session_id": "older-distinct-session",
		"completed_at": dbfx.Raw("now() - interval '2 minutes'"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "completed", "session_id": "legacy-poisoned",
		"completed_at": dbfx.Raw("now() - interval '1 minute'"),
	})
	failedID := fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "failed", "session_id": "legacy-poisoned",
		"failure_reason": "agent_error.provider_network", "completed_at": dbfx.Raw("now()"),
		"error": "stream disconnected before completion: RESPONSE PROTECTION IS UNAVAILABLE",
	})
	prior, err := db.New(pool).GetLastTaskSession(ctx, db.GetLastTaskSessionParams{
		AgentID: util.MustParseUUID(agentID), IssueID: util.MustParseUUID(issueID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("legacy poisoned session is resumable: session=%q err=%v", prior.SessionID.String, err)
	}
	queries := db.New(pool)
	parent, err := queries.GetAgentTask(ctx, util.MustParseUUID(failedID))
	if err != nil {
		t.Fatal(err)
	}
	svc := &TaskService{Queries: queries, TxStarter: pool, Bus: events.New()}
	fixture.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id = $1`, failedID)
	child, err := svc.MaybeRetryFailedTask(ctx, parent)
	if err != nil || child != nil {
		t.Errorf("sweeper must not retry a legacy poisoned failure: child=%+v err=%v", child, err)
	}
}

func TestGatewayFailureCooldownDoesNotBlockOtherAgent(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	fixture := dbfx.New(pool, workspaceID, userID)
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	otherAgentID := fixture.Agent(t, "another provider agent", runtimeID)
	fixture.Task(t, otherAgentID, dbfx.Cols{
		"runtime_id": runtimeID, "status": "failed", "failure_reason": "agent_error.provider_server_error",
		"error": "unexpected status 503 Service Unavailable", "completed_at": dbfx.Raw("now()"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "status": "failed", "failure_reason": "agent_error.provider_server_error",
		"completed_at": dbfx.Raw("now() - interval '11 minutes'"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "status": "failed", "failure_reason": "agent_error.provider_network",
		"error": "stream disconnected before completion", "completed_at": dbfx.Raw("now()"),
	})
	queuedID := fixture.Task(t, agentID, dbfx.Cols{"runtime_id": runtimeID, "issue_id": issueID})
	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	claimed, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil || claimed == nil || claimed.ID != util.MustParseUUID(queuedID) {
		t.Fatalf("unaffected agent must still claim: task=%+v err=%v", claimed, err)
	}
}

func TestResponseProtectionLegacyChatSessionSelection(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	fixture := dbfx.New(pool, workspaceID, userID)
	chatID := fixture.Insert(t, "chat_session", dbfx.Cols{
		"workspace_id": workspaceID, "creator_id": userID, "agent_id": agentID, "title": "poisoned chat",
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"chat_session_id": chatID, "status": "completed", "session_id": "older-distinct-chat-session",
		"completed_at": dbfx.Raw("now() - interval '2 minutes'"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"chat_session_id": chatID, "status": "completed", "session_id": "legacy-chat",
		"completed_at": dbfx.Raw("now() - interval '1 minute'"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"chat_session_id": chatID, "status": "failed", "session_id": "legacy-chat",
		"failure_reason": "agent_error.provider_network", "completed_at": dbfx.Raw("now()"),
		"error": "stream disconnected before completion: response protection is unavailable",
	})
	prior, err := db.New(pool).GetLastChatTaskSession(ctx, db.GetLastChatTaskSessionParams{ChatSessionID: util.MustParseUUID(chatID)})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("legacy poisoned chat is resumable: session=%q err=%v", prior.SessionID.String, err)
	}
}
