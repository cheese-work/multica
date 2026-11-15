package main

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
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

// newRuleEFixture is newCompactFixture on a dedicated agent and runtime. The
// shared workspace agent carries other tests' leftover tasks and concurrency
// slots, which a claim here would pick up instead of the task under test.
func newRuleEFixture(t *testing.T) compactFixture {
	t.Helper()
	var runtimeID, agentID, sessionID string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
		VALUES ($1, NULL, 'rule-e runtime', 'local', 'che2899_rule_e', 'online', '{}'::jsonb, '{}'::jsonb, now())
		RETURNING id::text`, testWorkspaceID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
		VALUES ($1, 'che2899-rule-e-agent', '', 'local', '{}'::jsonb, $2, 'workspace', 20, $3)
		RETURNING id::text`, testWorkspaceID, runtimeID, testUserID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, session_id, runtime_id, work_dir)
		VALUES ($1, $2, $3, 'compact', 'S_old', $4, '/tmp/compact-work') RETURNING id::text`,
		testWorkspaceID, agentID, testUserID, runtimeID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx(), `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
		testPool.Exec(ctx(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
		testPool.Exec(ctx(), `DELETE FROM agent WHERE id = $1`, agentID)
		testPool.Exec(ctx(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})
	token := chatTaskToken(t, sessionID, "running")
	var taskID string
	if err := testPool.QueryRow(ctx(), `SELECT id::text FROM agent_task_queue WHERE chat_session_id = $1`, sessionID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	return compactFixture{sessionID: sessionID, taskID: taskID, token: token}
}

// (p) A task other than the caller that is dispatched, waiting_local_directory
// or running blocks the compact with 409; a queued one does not.
func TestChatCompactRefusesWhileAnotherChatTaskIsActive(t *testing.T) {
	for _, active := range []string{"dispatched", "waiting_local_directory", "running"} {
		t.Run(active, func(t *testing.T) {
			fx := newRuleEFixture(t)
			addRuleEChatTask(t, fx.sessionID, active)
			if status, body := postCompact(t, fx.token, "x"); status != http.StatusConflict {
				t.Fatalf("compact beside a %s task = %d %s, want 409", active, status, body)
			}
			assertCompactUntouched(t, fx.sessionID)
		})
	}
	t.Run("queued does not block", func(t *testing.T) {
		fx := newRuleEFixture(t)
		addRuleEChatTask(t, fx.sessionID, "queued")
		if status, body := postCompact(t, fx.token, "x"); status != http.StatusOK {
			t.Fatalf("compact beside a queued task = %d %s, want 200", status, body)
		}
	})
}

// (q) opposite order: the claim commits first, the compact then answers 409
// and the DM epoch stays put.
func TestChatCompactRefusesTaskClaimedJustBefore(t *testing.T) {
	fx := newRuleEFixture(t)
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

// (q, two agents) agent B's queued DM task is claimed once agent A's task has
// settled; a compact from A's settled task then answers 403 or 409 and the DM
// epoch stays put. A second active row for the DM cannot exist any more (the
// one-active-per-DM index), so the refusal comes from the caller check.
func TestChatCompactRefusesAfterOtherAgentClaimsJustBefore(t *testing.T) {
	fx := newRuleEFixture(t)
	b := newAdmissionAgent(t, "q")
	queued := addAdmissionTask(t, b, fx.sessionID, "", "queued")
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fx.taskID); err != nil {
		t.Fatal(err)
	}
	claimNow(t, b.agentID, b.runtimeID)
	if got := taskStatus(t, queued); got != "dispatched" {
		t.Fatalf("agent B task = %q, want dispatched", got)
	}
	if status, body := postCompact(t, fx.token, "x"); status != http.StatusConflict && status != http.StatusForbidden {
		t.Fatalf("compact after the other agent's claim = %d %s, want 403 or 409", status, body)
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

// (s) lock order, real paths: the compact handler (DM, then task NOWAIT),
// CompleteTaskWithRunContext (DM, then task) and the service claim
// (ClaimTaskForRuntime) race behind a barrier. Nothing deadlocks or errors, the
// caller completes, the queued task is claimable afterwards, and a claimed
// task's epoch stamp equals the DM epoch (Rule E).
func TestChatCompactClaimLockOrderHasNoDeadlock(t *testing.T) {
	lockOrderRace(t, false)
}

// (s, two agents) the same race, but the queued successor belongs to another
// agent on its own runtime (CHE-2894 step 8).
func TestChatCompactClaimLockOrderHasNoDeadlockTwoAgents(t *testing.T) {
	lockOrderRace(t, true)
}

func lockOrderRace(t *testing.T, otherAgent bool) {
	svc := service.NewTaskService(db.New(testPool), testPool, nil, events.New())
	for i := 0; i < 40; i++ {
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			fx := newRuleEFixture(t)
			var queued, runtimeID string
			if otherAgent {
				b := newAdmissionAgent(t, fmt.Sprintf("s%d", i))
				queued = addAdmissionTask(t, b, fx.sessionID, "", "queued")
				runtimeID = b.runtimeID
			} else {
				queued = addRuleEChatTask(t, fx.sessionID, "queued")
				// Each subtest's fixture cleanup frees the agent's concurrency slot.
				if err := testPool.QueryRow(ctx(), `SELECT runtime_id FROM agent_task_queue WHERE id = $1`, queued).Scan(&runtimeID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := testPool.Exec(ctx(), `UPDATE agent_runtime SET status = 'online', last_seen_at = now() WHERE id = $1`, runtimeID); err != nil {
				t.Fatal(err)
			}
			claim := func() (claimedQueued bool, err error) {
				for k := 0; k < 10; k++ {
					got, err := svc.ClaimTaskForRuntime(ctx(), ruleEUUID(t, runtimeID))
					if err != nil || got == nil {
						return claimedQueued, err
					}
					if util.UUIDToString(got.ID) == queued {
						return true, nil
					}
				}
				return false, nil
			}

			var wg sync.WaitGroup
			start := make(chan struct{})
			var compactCode int
			var compactBody []byte
			var completeErr, claimErr error
			var claimedInRace bool
			wg.Add(3)
			go func() {
				defer wg.Done()
				<-start
				compactCode, compactBody = postCompact(t, fx.token, "x")
			}()
			go func() {
				defer wg.Done()
				<-start
				_, _, completeErr = svc.CompleteTaskWithRunContext(ctx(), ruleEUUID(t, fx.taskID), []byte(`{"output":"done"}`), "", "", "", false, "", "", nil)
			}()
			go func() {
				defer wg.Done()
				<-start
				claimedInRace, claimErr = claim()
			}()
			close(start)
			finished := make(chan struct{})
			go func() { wg.Wait(); close(finished) }()
			select {
			case <-finished:
			case <-time.After(30 * time.Second):
				t.Fatalf("iteration %d: compact, completion and claim did not finish (deadlock)", i)
			}

			if completeErr != nil {
				t.Fatalf("iteration %d: complete caller: %v", i, completeErr)
			}
			if claimErr != nil {
				t.Fatalf("iteration %d: claim: %v", i, claimErr)
			}
			// 403: the caller completed before the handler read it. 409: the claim won.
			if compactCode != http.StatusOK && compactCode != http.StatusConflict && compactCode != http.StatusForbidden {
				t.Fatalf("iteration %d: compact = %d %s, want 200, 403 or 409", i, compactCode, compactBody)
			}
			var callerStatus string
			if err := testPool.QueryRow(ctx(), `SELECT status FROM agent_task_queue WHERE id = $1`, fx.taskID).Scan(&callerStatus); err != nil {
				t.Fatal(err)
			}
			if callerStatus != "completed" {
				t.Fatalf("iteration %d: caller ended %q, want completed", i, callerStatus)
			}
			if !claimedInRace {
				if _, err := claim(); err != nil {
					t.Fatalf("iteration %d: claim after the race: %v", i, err)
				}
			}
			var status string
			var stamp *int64
			var epoch int64
			if err := testPool.QueryRow(ctx(), `
				SELECT atq.status, atq.chat_compaction_epoch, cs.compaction_epoch
				FROM agent_task_queue atq JOIN chat_session cs ON cs.id = atq.chat_session_id WHERE atq.id = $1`, queued).Scan(&status, &stamp, &epoch); err != nil {
				t.Fatal(err)
			}
			if status != "dispatched" {
				t.Fatalf("iteration %d: queued task ended %q, want dispatched after the race", i, status)
			}
			if stamp == nil || *stamp != epoch {
				t.Fatalf("iteration %d: claimed task epoch %v, DM epoch %d (compact = %d)", i, stamp, epoch, compactCode)
			}
			if (compactCode == http.StatusOK) != (epoch == 1) {
				t.Fatalf("iteration %d: compact = %d but DM epoch = %d", i, compactCode, epoch)
			}
		})
	}
}
