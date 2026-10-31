package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
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

// memberRequester makes the issue's requester a plain workspace member, who
// does not own the workspace and cannot invoke the private assignee agent.
func (c budgetCase) memberRequester(t *testing.T, email string) string {
	t.Helper()
	userID := dbfx.User(t, "Budget Member", email)
	dbfx.Member(t, testWorkspaceID, userID, "member")
	dbfx.Exec(t, `UPDATE issue SET requested_by_id=$2 WHERE id=$1`, c.issueID, userID)
	return userID
}

// writeExhausted delivers the exhaustion notice for the current state of the
// issue, as the admission hook does after a refusal.
func (c budgetCase) writeExhausted(t *testing.T) {
	t.Helper()
	e := service.RunBudgetEvent{IssueID: parseUUID(c.issueID), Kind: service.RunBudgetKindExhausted, Runs: 2, Max: 2, Since: time.Now().Add(-time.Hour)}
	testHandler.writeRunBudgetNotice(context.Background(), e, "This issue used up its run budget.")
}

func (c budgetCase) requestRow(t *testing.T) (id, human string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `SELECT id::text, human_user_id::text FROM decision_request WHERE issue_id=$1 AND request_fingerprint LIKE 'server:run_budget:%'`, c.issueID).Scan(&id, &human); err != nil {
		t.Fatalf("server-origin request: %v", err)
	}
	return id, human
}

// A recipient gets the request whether or not the issue has an agent assignee
// and whether or not the recipient could invoke that agent.
func TestRunBudgetDecisionRequestDoesNotDependOnAssignee(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Run("requester cannot invoke the assignee", func(t *testing.T) {
		c := newBudgetCase(t, 2318008, mergeEdgeSteps, 2)
		member := c.memberRequester(t, "che2318-noinvoke@multica.test")
		c.spend(t, 2)
		c.refuse(t)
		if n := c.waitDecisions(t, 1); n != 1 {
			t.Fatalf("decision requests = %d, want 1", n)
		}
		if _, human := c.requestRow(t); human != member {
			t.Fatalf("request is for %s, want the requester %s", human, member)
		}
	})
	t.Run("assignee is archived", func(t *testing.T) {
		c := newBudgetCase(t, 2318009, mergeEdgeSteps, 2)
		c.spend(t, 2)
		dbfx.Agent(t, "Budget Fallback Asker", handlerTestRuntimeID(t))
		dbfx.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, c.agentID)
		t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET archived_at=NULL WHERE id=$1`, c.agentID) })
		c.writeExhausted(t)
		if n := c.decisionRows(t); n != 1 {
			t.Fatalf("decision requests = %d, want 1", n)
		}
	})
	t.Run("issue has no assignee", func(t *testing.T) {
		c := newBudgetCase(t, 2318010, mergeEdgeSteps, 2)
		c.spend(t, 2)
		dbfx.Exec(t, `UPDATE issue SET assignee_type=NULL, assignee_id=NULL WHERE id=$1`, c.issueID)
		c.writeExhausted(t)
		if n := c.decisionRows(t); n != 1 {
			t.Fatalf("decision requests = %d, want 1", n)
		}
	})
}

// The lifecycle worker cancels an ordinary non-owner request; the platform's own
// request to a plain member must survive a pass.
func TestRunBudgetDecisionRequestSurvivesLifecyclePass(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDecisionLease(t)
	// Not newDecisionRunnerFixture: a second Handler would take over the global
	// budget hook and write later tests' notices to its own event bus.
	runner := decisionRunnerFor(t, testHandler)
	for name, delegate := range map[string]bool{"plain member": false, "project delegate": true} {
		t.Run(name, func(t *testing.T) {
			c := newBudgetCase(t, 2318011+int64(len(name)), mergeEdgeSteps, 2)
			member := c.memberRequester(t, "che2318-"+strings.ReplaceAll(name, " ", "-")+"@multica.test")
			c.spend(t, 2)
			c.refuse(t)
			c.waitDecisions(t, 1)
			if delegate {
				// Moving the issue into a project that names the member a delegate
				// is what makes the ordinary lifecycle rule apply to them.
				placeDecisionHuman(t, c.issueID, member, "member", true)
			}
			id, _ := c.requestRow(t)
			decisionPass(t, runner)
			decisionPass(t, runner)
			if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id=$1 AND status='open'`, id); n != 1 {
				t.Fatal("lifecycle pass cancelled the server-origin request")
			}
		})
	}
}

// A project delegate may raise an ordinary reversible request above their own
// authority, which cancels it; they must not be able to do that to this one.
func TestRunBudgetDecisionRequestRefusesClassChange(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2318020, mergeEdgeSteps, 2)
	member := c.memberRequester(t, "che2318-class@multica.test")
	c.spend(t, 2)
	c.refuse(t)
	c.waitDecisions(t, 1)
	placeDecisionHuman(t, c.issueID, member, "member", true)
	id, _ := c.requestRow(t)
	for _, class := range []string{"spend", "irreversible", "product_direction"} {
		request := newRequest(http.MethodPost, "/api/decision-requests/"+id+"/class", map[string]any{"escalation_class": class})
		request.Header.Set("X-User-ID", member)
		request.Header.Set("X-Actor-Source", "interactive_jwt")
		response := httptest.NewRecorder()
		testHandler.RaiseDecisionClass(response, withIssueParam(request, id))
		if response.Code != http.StatusForbidden {
			t.Fatalf("raise to %s: status %d body %s, want 403", class, response.Code, response.Body.String())
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id=$1 AND status='open' AND escalation_class='reversible'`, id); n != 1 {
		t.Fatal("class change touched the server-origin request")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_class_change WHERE decision_request_id=$1`, id); n != 0 {
		t.Fatalf("class changes recorded = %d, want 0", n)
	}
}

// Creating the notice takes the decision locks in the standard order, so
// concurrent refusals, lifecycle passes and a retried delivery neither deadlock
// nor duplicate the request.
func TestRunBudgetDecisionRequestLockOrder(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDecisionLease(t)
	// Not newDecisionRunnerFixture: a second Handler would take over the global
	// budget hook and write later tests' notices to its own event bus.
	runner := decisionRunnerFor(t, testHandler)
	c := newBudgetCase(t, 2318030, mergeEdgeSteps, 2)
	c.memberRequester(t, "che2318-lock@multica.test")
	c.spend(t, 2)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.admit(t)
		}()
		go func() {
			defer wg.Done()
			decisionPass(t, runner)
		}()
	}
	wg.Wait()
	if n := c.waitDecisions(t, 1); n != 1 {
		t.Fatalf("decision requests = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id=$1 AND status='open'`, c.issueID); n != 1 {
		t.Fatalf("open requests = %d, want 1", n)
	}
}

// The DM events go to the person the session belongs to, not to the asking
// agent's id, and the inserted decision message reaches an open lane.
func TestRunBudgetDecisionRequestChatEventsRouteToRecipient(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	// Another test's second Handler may hold the global budget hooks, which
	// would publish to its own bus; take them back for the shared handler.
	service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted)
	service.SetOnRunBudgetEvent(testHandler.onRunBudgetEvent)
	var mutex sync.Mutex
	var seen []events.Event
	collect := func(event events.Event) {
		mutex.Lock()
		defer mutex.Unlock()
		seen = append(seen, event)
	}
	testHandler.Bus.Subscribe(protocol.EventChatSessionCreated, collect)
	testHandler.Bus.Subscribe(protocol.EventChatMessage, collect)
	c := newBudgetCase(t, 2318040, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitDecisions(t, 1)
	var session, human string
	if err := testPool.QueryRow(context.Background(), `SELECT chat_session_id::text, human_user_id::text FROM decision_request WHERE issue_id=$1`, c.issueID).Scan(&session, &human); err != nil {
		t.Fatal(err)
	}
	// The events publish after the commit the row count above already sees.
	var created, messages int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		created, messages = 0, 0
		mutex.Lock()
		for _, event := range seen {
			if event.ChatSessionID != session {
				continue
			}
			if event.ChatSessionOwnerID != human {
				mutex.Unlock()
				t.Fatalf("%s owner = %q, want recipient %q", event.Type, event.ChatSessionOwnerID, human)
			}
			if event.Type == protocol.EventChatSessionCreated {
				created++
			} else {
				messages++
			}
		}
		mutex.Unlock()
		if created > 0 && messages > 0 {
			break
		}
	}
	if created != 1 || messages < 1 {
		t.Fatalf("recipient chat events: session-created %d, message %d, want 1 and at least 1", created, messages)
	}
}
