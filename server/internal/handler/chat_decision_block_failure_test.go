package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/analytics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decisionReadFailDBTX passes every statement through except the eligible-ask
// read, which it fails.
type decisionReadFailDBTX struct{ inner db.DBTX }

var errInjectedDecisionRead = errors.New("injected decision request read failure")

func (f decisionReadFailDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.inner.Exec(ctx, sql, args...)
}

func (f decisionReadFailDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM decision_request request") {
		return nil, errInjectedDecisionRead
	}
	return f.inner.Query(ctx, sql, args...)
}

func (f decisionReadFailDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return f.inner.QueryRow(ctx, sql, args...)
}

// A failed eligible-ask read must not become "no asks" (the run would then be
// told nothing is open) and must not consume the task: the claim answers 500
// and parks the task at dispatched, still eligible for stale-dispatched reclaim.
func TestClaimChatDecisionReadErrorKeepsTaskForRedelivery(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "decision read error chat")
	seedDMDecision(t, agentID, sessionID, "Ship or hold?", "open", "2 hours")
	taskID := sendDirectChat(t, ctx, agentID, sessionID, "A")

	h := New(db.New(decisionReadFailDBTX{inner: testPool}), testPool, testHandler.Hub, testHandler.Bus, testHandler.EmailService, nil, nil, analytics.NoopClient{}, Config{})
	claimForRuntimeExpecting(t, h, runtimeID, daemonID, http.StatusInternalServerError)

	var status string
	var startedAt *string
	dbfx.QueryRow(t, `SELECT status, started_at::text FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status, &startedAt)
	if status != "dispatched" || startedAt != nil {
		t.Errorf("task = %q started_at %v, want dispatched with started_at NULL (parked for redelivery)", status, startedAt)
	}
	// The recovered claim then delivers the ask.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'queued', dispatched_at = NULL WHERE id = $1`, taskID)
	if carrier := claimForkTask(t, runtimeID, daemonID).ChatDecisionContext; !strings.Contains(carrier, "Ship or hold?") {
		t.Errorf("redelivered claim lost the ask:\n%s", carrier)
	}
}
