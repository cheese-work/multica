package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6720: a busy_retry answer names the step that refused, so the next
// occurrence is diagnosable without the server log. One case per lock an
// approval of a weekly run guard card can meet.
func TestApproveRunGuardCardBusyRetryBodyNamesTheRefusingStep(test *testing.T) {
	if testHandler == nil {
		test.Skip("database not available")
	}
	delays := decisionActionRetryDelays
	decisionActionRetryDelays = []time.Duration{time.Millisecond}
	test.Cleanup(func() { decisionActionRetryDelays = delays })
	const request = `(SELECT decision_request_id FROM decision_action WHERE id = $1)`
	for _, held := range []struct{ step, query string }{
		{"chat_session", `SELECT 1 FROM chat_session WHERE id = (SELECT chat_session_id FROM decision_request WHERE id = ` + request + `) FOR UPDATE`},
		{"issue", `SELECT 1 FROM issue WHERE id = (SELECT issue_id FROM decision_request WHERE id = ` + request + `) FOR UPDATE`},
		{"request", `SELECT 1 FROM decision_request WHERE id = ` + request + ` FOR UPDATE`},
		{"executor", `SELECT 1 FROM weekly_run_guard_refusal WHERE action_id IS NULL AND issue_id = (SELECT issue_id FROM decision_request WHERE id = ` + request + `) FOR UPDATE`},
	} {
		test.Run(held.step, func(test *testing.T) {
			c, _ := weeklyCardCase(test, 6720100)
			action := c.pendingAllow(test)
			holdRow(test, held.query, action.actionID)
			verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return pgtype.UUID{}, nil }
			body := action.approve(test, withVerifier(verify), action.claim(test, map[string]string{}), http.StatusConflict)
			var answer struct{ Error, Step string }
			if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Step != held.step {
				test.Fatalf("body %q: step = %q, want %q (%v)", body, answer.Step, held.step, err)
			}
		})
	}
}

// CHE-6720: the C00 log named step=agents. The asking agent is only read by an
// approval, and every decision path (delivery, lifecycle, wake receipt, class
// raise) holds it FOR SHARE, so a share holder on a hot agent must not refuse
// the click, however long it holds.
func TestApproveRunGuardCardIgnoresAShareLockOnTheAskingAgent(test *testing.T) {
	if testHandler == nil {
		test.Skip("database not available")
	}
	delays := decisionActionRetryDelays
	decisionActionRetryDelays = []time.Duration{time.Millisecond}
	test.Cleanup(func() { decisionActionRetryDelays = delays })
	c, _ := weeklyCardCase(test, 6720200)
	action := c.pendingAllow(test)
	holdRow(test, `SELECT 1 FROM agent WHERE id = (SELECT requested_by_agent_id FROM decision_action WHERE id = $1) FOR SHARE`, action.actionID)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return pgtype.UUID{}, nil }
	action.approve(test, withVerifier(verify), action.claim(test, map[string]string{}), http.StatusOK)
}

// CHE-6720: the real long holder. claimTask takes the agent row FOR UPDATE and
// then waits for the runtime's claim-capacity advisory lock, which serializes
// every agent on the runtime, so the agent row stays locked for as long as that
// queue is. An approval does not write the asking agent and must not wait on it.
func TestApproveRunGuardCardIgnoresAClaimQueuedOnTheRuntime(test *testing.T) {
	if testHandler == nil {
		test.Skip("database not available")
	}
	delays := decisionActionRetryDelays
	decisionActionRetryDelays = []time.Duration{time.Millisecond}
	test.Cleanup(func() { decisionActionRetryDelays = delays })
	ctx := context.Background()
	c, _ := weeklyCardCase(test, 6720300)
	action := c.pendingAllow(test)
	var agentID, runtimeID pgtype.UUID
	if err := testPool.QueryRow(ctx, `SELECT a.id, a.runtime_id FROM agent a WHERE a.id = (SELECT requested_by_agent_id FROM decision_action WHERE id = $1)`, action.actionID).Scan(&agentID, &runtimeID); err != nil || !runtimeID.Valid {
		test.Fatalf("asking agent runtime: %v (valid=%v)", err, runtimeID.Valid)
	}
	queue, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer queue.Rollback(ctx)
	if err := testHandler.Queries.WithTx(queue).LockRuntimeClaimCapacity(ctx, runtimeID); err != nil {
		test.Fatal(err)
	}
	claimed := make(chan error, 1)
	go func() { _, err := testHandler.TaskService.ClaimTask(ctx, agentID); claimed <- err }()
	waitForLockWaiters(test, 1)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return pgtype.UUID{}, nil }
	// The approval's own post-commit work needs the runtime lock too, so the
	// queue is released once the action has committed (or the click was refused).
	approved := make(chan struct{})
	go func() {
		defer close(approved)
		action.approve(test, withVerifier(verify), action.claim(test, map[string]string{}), http.StatusOK)
	}()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var state string
		if err := testPool.QueryRow(ctx, `SELECT state FROM decision_action WHERE id = $1`, action.actionID).Scan(&state); err != nil {
			test.Fatal(err)
		}
		select {
		case <-approved:
		default:
			if state == "pending" {
				continue
			}
		}
		break
	}
	_ = queue.Rollback(ctx)
	<-approved
	if err := <-claimed; err != nil {
		test.Fatalf("claim: %v", err)
	}
}
