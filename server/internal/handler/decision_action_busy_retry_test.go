package handler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const holdAskingAgent = `SELECT 1 FROM agent WHERE id = (SELECT requested_by_agent_id FROM decision_action WHERE id = $1) FOR UPDATE`

// holdRowUntil locks a row in another transaction and releases it after delay.
func holdRowUntil(test *testing.T, delay time.Duration, query string, args ...any) {
	test.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		_ = tx.Rollback(ctx)
		test.Fatal(err)
	}
	released := make(chan struct{})
	time.AfterFunc(delay, func() { _ = tx.Rollback(ctx); close(released) })
	test.Cleanup(func() { <-released })
}

// A click that meets a short-lived row lock waits it out (CHE-5391).
func TestApproveDecisionActionRetriesWhileTheAskingAgentIsBrieflyLocked(test *testing.T) {
	action := newPendingSecretAction(test)
	holdRowUntil(test, 500*time.Millisecond, holdAskingAgent, action.actionID)
	var out decisionActionOutcome
	decodeJSON(test, action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK), &out)
	if out.State != "executed" {
		test.Fatalf("outcome = %+v", out)
	}
}

func TestRejectDecisionActionRetriesWhileTheAskingAgentIsBrieflyLocked(test *testing.T) {
	action := newPendingAction(test)
	holdRowUntil(test, 500*time.Millisecond, holdAskingAgent, action.actionID)
	action.reject(test, testHandler, http.StatusOK)
	if state := action.state(test); state != "rejected" {
		test.Fatalf("stored state = %s", state)
	}
}

// A lock held past every retry still answers busy_retry, names the refusing
// step in the log, and never reaches the passkey verifier.
func TestApproveDecisionActionBusyRetryNamesTheStepAndSkipsThePasskey(test *testing.T) {
	action := newPendingSecretAction(test)
	holdRow(test, holdAskingAgent, action.actionID)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	test.Cleanup(func() { slog.SetDefault(previous) })
	var verified atomic.Int32
	verify := func(ctx context.Context, queries *db.Queries, claim ApprovalClaim) (pgtype.UUID, error) {
		verified.Add(1)
		return pgtype.UUID{}, nil
	}
	body := action.approve(test, withVerifier(verify), action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusConflict)
	if !strings.Contains(body, "busy_retry") {
		test.Fatalf("body %q lacks busy_retry", body)
	}
	if verified.Load() != 0 {
		test.Fatalf("the passkey verifier ran %d times behind a refused lock", verified.Load())
	}
	for _, want := range []string{"busy_retry", "step=agents", "action_id=" + action.actionID, "issue_id=" + action.issueID} {
		if !strings.Contains(logs.String(), want) {
			test.Fatalf("log lacks %q:\n%s", want, logs.String())
		}
	}
	action.assertUntouched(test)
}
