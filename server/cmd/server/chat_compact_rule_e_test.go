package main

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Rule E (CHE-2819): compact is exclusive on the DM. While a chat task other
// than the caller is claimed or running, the epoch must not move.

func addRuleEChatTask(t *testing.T, sessionID, status string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status)
		SELECT cs.agent_id, a.runtime_id, cs.id, $2 FROM chat_session cs JOIN agent a ON a.id = cs.agent_id
		WHERE cs.id = $1 RETURNING id`, sessionID, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// (p) A task other than the caller that is dispatched, waiting_local_directory
// or running blocks the compact with 409; a queued one does not.
func TestChatCompactRefusesWhileAnotherChatTaskIsActive(t *testing.T) {
	for _, active := range []string{"dispatched", "waiting_local_directory", "running"} {
		t.Run(active, func(t *testing.T) {
			fx := newCompactFixture(t)
			addRuleEChatTask(t, fx.sessionID, active)
			if status, body := postCompact(t, fx.token, "x"); status != http.StatusConflict {
				t.Fatalf("compact beside a %s task = %d %s, want 409", active, status, body)
			}
			assertCompactUntouched(t, fx.sessionID)
		})
	}
	t.Run("queued does not block", func(t *testing.T) {
		fx := newCompactFixture(t)
		addRuleEChatTask(t, fx.sessionID, "queued")
		if status, body := postCompact(t, fx.token, "x"); status != http.StatusOK {
			t.Fatalf("compact beside a queued task = %d %s, want 200", status, body)
		}
	})
}

// (q) opposite order: the claim commits first, the compact then answers 409
// and the DM epoch stays put.
func TestChatCompactRefusesTaskClaimedJustBefore(t *testing.T) {
	fx := newCompactFixture(t)
	queued := addRuleEChatTask(t, fx.sessionID, "queued")
	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx(), `SELECT agent_id, runtime_id FROM agent_task_queue WHERE id = $1`, queued).Scan(&agentID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	// The fixture caller is still running, so serialization keeps the queued
	// task back; finish it to let the claim win, as for a human compact.
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fx.taskID); err != nil {
		t.Fatal(err)
	}
	claimNow(t, agentID, runtimeID)
	callerAgain := chatTaskToken(t, fx.sessionID, "running")
	if status, body := postCompact(t, callerAgain, "x"); status != http.StatusConflict {
		t.Fatalf("compact after a fresh claim = %d %s, want 409", status, body)
	}
	assertCompactUntouched(t, fx.sessionID)
}

func ruleEUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	u, err := util.ParseUUID(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func claimNow(t *testing.T, agentID, runtimeID string) *db.AgentTaskQueue {
	t.Helper()
	if _, err := testPool.Exec(ctx(), `UPDATE agent_runtime SET status = 'online', last_seen_at = now() WHERE id = $1`, runtimeID); err != nil {
		t.Fatal(err)
	}
	task, err := db.New(testPool).ClaimAgentTask(ctx(), db.ClaimAgentTaskParams{
		AgentID: ruleEUUID(t, agentID), RuntimeID: ruleEUUID(t, runtimeID), PrepareLeaseSecs: 30, RuntimeStaleSecs: 3600,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return &task
}

// (s) lock order: compact (DM, then task NOWAIT), a task completing and claim
// polls run in a barrier loop. No deadlock, no 5xx, and the queued task is
// never lost.
func TestChatCompactClaimLockOrderHasNoDeadlock(t *testing.T) {
	for i := 0; i < 25; i++ {
		fx := newCompactFixture(t)
		queued := addRuleEChatTask(t, fx.sessionID, "queued")
		var agentID, runtimeID string
		if err := testPool.QueryRow(ctx(), `SELECT agent_id, runtime_id FROM agent_task_queue WHERE id = $1`, queued).Scan(&agentID, &runtimeID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make(chan int, 1)
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			status, _ := postCompact(t, fx.token, "x")
			codes <- status
		}()
		go func() {
			defer wg.Done()
			<-start
			testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, fx.taskID)
		}()
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 5; j++ {
				db.New(testPool).ClaimAgentTask(ctx(), db.ClaimAgentTaskParams{
					AgentID: ruleEUUID(t, agentID), RuntimeID: ruleEUUID(t, runtimeID), PrepareLeaseSecs: 30, RuntimeStaleSecs: 3600,
				})
			}
		}()
		close(start)
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Fatalf("iteration %d: compact, completion and claim did not finish (deadlock)", i)
		}
		// 403 when the caller completed before the handler read it.
		if code := <-codes; code != http.StatusOK && code != http.StatusConflict && code != http.StatusForbidden {
			t.Fatalf("iteration %d: compact = %d, want 200, 403 or 409", i, code)
		}
		var status string
		if err := testPool.QueryRow(ctx(), `SELECT status FROM agent_task_queue WHERE id = $1`, queued).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "queued" && status != "dispatched" {
			t.Fatalf("iteration %d: queued task ended %q, want queued or dispatched", i, status)
		}
	}
}
