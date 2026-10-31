package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// decisionRows counts the issue's server-origin exhaustion decision requests.
func (c budgetCase) decisionRows(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id=$1 AND request_fingerprint LIKE 'server:run_budget:%'`, c.issueID)
}

// waitDecisions waits for the hook goroutine, then lets stray duplicates land.
func (c budgetCase) waitDecisions(t *testing.T, want int) int {
	t.Helper()
	c.waitNotices(t, service.RunBudgetKindExhausted, want)
	return c.decisionRows(t)
}

func TestRunBudgetExhaustionOpensOneDecisionRequest(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2318001, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	if n := c.waitDecisions(t, 1); n != 1 {
		t.Fatalf("decision requests = %d, want 1", n)
	}
	var human, ask, origin, status string
	var options, askJSON []byte
	if err := testPool.QueryRow(context.Background(), `SELECT human_user_id::text, ask, origin, status, options, ask_json FROM decision_request WHERE issue_id=$1`, c.issueID).
		Scan(&human, &ask, &origin, &status, &options, &askJSON); err != nil {
		t.Fatal(err)
	}
	if human != testUserID || origin != "asked" || status != "open" {
		t.Fatalf("request = human %s origin %s status %s, want requester %s asked open", human, origin, status, testUserID)
	}
	var opts []struct{ Label string }
	if err := json.Unmarshal(options, &opts); err != nil || len(opts) < 2 {
		t.Fatalf("options = %s (%v), want the reopen options", options, err)
	}
	var parsed struct {
		DefaultIfNoReply string `json:"default_if_no_reply"`
	}
	if err := json.Unmarshal(askJSON, &parsed); err != nil || !strings.Contains(strings.ToLower(parsed.DefaultIfNoReply), "no action") {
		t.Fatalf("default_if_no_reply = %q (%v), want a default of no action", parsed.DefaultIfNoReply, err)
	}
	if !strings.Contains(ask, "multica issue budget top-up") {
		t.Fatalf("ask does not name the top-up: %s", ask)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, c.issueID); n != 0 {
		t.Fatalf("open tasks = %d, want none behind a server-origin request", n)
	}
}

func TestRunBudgetDecisionRequestIsIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Run("concurrent refusals", func(t *testing.T) {
		c := newBudgetCase(t, 2318002, mergeEdgeSteps, 2)
		c.spend(t, 2)
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.admit(t)
			}()
		}
		wg.Wait()
		if n := c.waitDecisions(t, 1); n != 1 {
			t.Fatalf("decision requests = %d, want 1", n)
		}
	})
	t.Run("retried delivery", func(t *testing.T) {
		c := newBudgetCase(t, 2318003, mergeEdgeSteps, 2)
		c.spend(t, 2)
		c.refuse(t)
		c.waitDecisions(t, 1)
		c.refuse(t)
		if n := c.waitDecisions(t, 2); n != 1 {
			t.Fatalf("decision requests = %d, want 1", n)
		}
	})
}

func TestRunBudgetDecisionRequestWithoutStatusMove(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Run("already blocked", func(t *testing.T) {
		c := newBudgetCase(t, 2318004, mergeEdgeSteps, 2)
		c.spend(t, 2)
		c.enter(t, "blocked")
		c.refuse(t)
		if n := c.waitDecisions(t, 1); n != 1 {
			t.Fatalf("decision requests = %d, want 1", n)
		}
	})
	t.Run("workflow without blocked", func(t *testing.T) {
		c := newBudgetCase(t, 2318005, noBlockedSteps, 2)
		c.spend(t, 2)
		c.refuse(t)
		if n := c.waitDecisions(t, 1); n != 1 {
			t.Fatalf("decision requests = %d, want 1", n)
		}
	})
}

func TestRunBudgetNewWindowRearmsDecisionRequest(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2318006, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitDecisions(t, 1)
	if _, err := testPool.Exec(context.Background(), `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'member', $3, 'status_changed', '{}')`, testWorkspaceID, c.issueID, testUserID); err != nil {
		t.Fatal(err)
	}
	c.enter(t, "in_progress")
	c.spend(t, 2)
	c.refuse(t)
	if n := c.waitDecisions(t, 2); n != 2 {
		t.Fatalf("decision requests = %d, want 2 (one per window)", n)
	}
}

// No API caller can suppress, and none can create, the server-origin request.
func TestRunBudgetDecisionRequestRefusesApiCallers(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2318007, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	if n := c.waitDecisions(t, 1); n != 1 {
		t.Fatalf("decision requests = %d, want 1", n)
	}
	var requestID, askingAgent string
	if err := testPool.QueryRow(context.Background(), `SELECT id::text, asking_agent_id::text FROM decision_request WHERE issue_id=$1`, c.issueID).Scan(&requestID, &askingAgent); err != nil {
		t.Fatal(err)
	}
	withdraw := func(mutate func(*http.Request)) {
		t.Helper()
		request := withURLParam(newRequest(http.MethodPost, "/api/decisions/"+requestID+"/withdraw", map[string]any{"reason": "suppress the budget request"}), "id", requestID)
		mutate(request)
		testutil.Call(t, testHandler.WithdrawDecision, request).Want(http.StatusForbidden)
	}
	t.Run("named human cannot withdraw", func(t *testing.T) { withdraw(func(*http.Request) {}) })
	t.Run("asking agent task cannot withdraw", func(t *testing.T) {
		var taskID string
		if err := testPool.QueryRow(context.Background(), `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
			SELECT id, runtime_id, $2, 'running', 0 FROM agent WHERE id=$1 RETURNING id::text`, askingAgent, c.issueID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		withdraw(func(r *http.Request) {
			r.Header.Set("X-Actor-Source", "task_token")
			r.Header.Set("X-Agent-ID", askingAgent)
			r.Header.Set("X-Task-ID", taskID)
		})
	})
	t.Run("member session cannot create", func(t *testing.T) {
		before := c.decisionRows(t)
		request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": c.issueID, "for": testUserID, "ask": "Reopen the run budget now please", "escalation_class": "reversible"})
		testutil.Call(t, testHandler.RequestDecision, request).Want(http.StatusForbidden)
		if after := c.decisionRows(t); after != before {
			t.Fatalf("server-origin requests %d -> %d after a member call", before, after)
		}
	})
	t.Run("task token cannot create without a running task", func(t *testing.T) {
		request := newRequest(http.MethodPost, "/api/decisions", map[string]any{"issue_id": c.issueID, "for": testUserID, "ask": "Reopen the run budget now please", "escalation_class": "reversible"})
		request.Header.Set("X-Actor-Source", "task_token")
		request.Header.Set("X-Agent-ID", askingAgent)
		request.Header.Set("X-Task-ID", "00000000-0000-4000-8000-000000000000")
		if response := testutil.Call(t, testHandler.RequestDecision, request); response.Code < 400 {
			t.Fatalf("status = %d, want a refusal", response.Code)
		}
	})
	if status := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id=$1 AND status='open'`, requestID); status != 1 {
		t.Fatal("request is no longer open after the refused calls")
	}
}
