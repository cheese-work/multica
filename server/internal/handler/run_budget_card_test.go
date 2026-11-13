package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// topUpCards counts the issue's open top-up action cards.
func (c budgetCase) topUpCards(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id
		WHERE r.issue_id = $1 AND r.status = 'open' AND a.action_type = 'issue.run_budget.top_up'`, c.issueID)
}

// questionCards counts the retired server-origin budget question cards.
func (c budgetCase) questionCards(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND request_fingerprint LIKE 'server:run_budget:%'`, c.issueID)
}

// exhaustBudget spends the fixture issue's whole run cap (3) in its open window,
// so a card for that window still stands for a real refusal.
func exhaustBudget(t *testing.T, agentID, issueID string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "status": "completed", "runtime_id": handlerTestRuntimeID(t)})
	}
}

// A budget refusal opens one top-up action card and no question card.
func TestRunBudgetRefusalOpensOneTopUpActionCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2530301, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitNotices(t, "exhausted", 1)
	if n := c.topUpCards(t); n != 1 {
		t.Fatalf("open top-up action cards = %d, want 1", n)
	}
	if n := c.questionCards(t); n != 0 {
		t.Fatalf("budget question cards = %d, want 0", n)
	}
	var actionID, fingerprint string
	dbfx.QueryRow(t, `SELECT a.id::text, r.request_fingerprint FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id WHERE r.issue_id = $1`, c.issueID).Scan(&actionID, &fingerprint)
	if !strings.HasPrefix(fingerprint, "server:run_budget_action:top_up:") {
		t.Fatalf("fingerprint = %q, want a top_up action fingerprint", fingerprint)
	}
	var body string
	dbfx.QueryRow(t, `SELECT c.content FROM run_budget_notice n JOIN comment c ON c.id = n.comment_id WHERE n.issue_id = $1 AND n.kind = 'exhausted'`, c.issueID).Scan(&body)
	if !strings.Contains(body, "?approve="+actionID) {
		t.Fatalf("Blocked comment does not link the card (?approve=%s):\n%s", actionID, body)
	}
}

// A second refusal in the same window reuses the card.
func TestRunBudgetSecondRefusalReusesTheCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2530302, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitNotices(t, "exhausted", 1)
	c.refuse(t)
	c.waitNotices(t, "exhausted", 2)
	if n := c.topUpCards(t); n != 1 {
		t.Fatalf("open top-up action cards after two refusals = %d, want 1", n)
	}
}

// decision ask about a budget block is refused while the card is open.
func TestDecisionAskOnABudgetBlockedIssueReturnsBudgetCardOpen(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newBudgetRequestFixture(t)
	dbfx.Exec(t, `UPDATE issue SET requested_by_id = $1 WHERE id = $2`, fixture.humanID, fixture.issueID)
	exhaustBudget(t, fixture.agentID, fixture.issueID)
	var card budgetRequestResponse
	fixture.request(t, fixture.humanID, "interactive_jwt", map[string]any{"kind": "top_up", "window_id": fixture.window}).Want(http.StatusCreated).JSON(&card)

	for name, handler := range map[string]http.HandlerFunc{"ask": testHandler.AskDecision, "request": testHandler.RequestDecision} {
		t.Run(name, func(t *testing.T) {
			body := fixture.askBody()
			request := fixture.askCall(body)
			if name == "request" {
				body["issue_id"], body["for"] = fixture.issueID, fixture.humanID
				request = fixture.call(body)
			}
			var got struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			testutil.Call(t, handler, request).Want(http.StatusConflict).JSON(&got)
			if got.Code != "budget_card_open" || !strings.Contains(got.Error, card.ApproveURL) {
				t.Fatalf("refusal = %+v, want budget_card_open naming %s", got, card.ApproveURL)
			}
		})
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND request_fingerprint NOT LIKE 'server:%'`, fixture.issueID); n != 0 {
		t.Fatalf("refused calls wrote %d requests", n)
	}
}

func completeAsAgent(t *testing.T, agentID, issueID string) error {
	t.Helper()
	ctx := context.WithValue(context.Background(), wakeupActorKey{}, wakeupActor{kind: "agent", id: agentID})
	return guardAgentIssueStatus(ctx, testHandler.Queries, db.UpdateIssueParams{ID: parseUUID(issueID), Status: pgtype.Text{String: "done", Valid: true}})
}

// An answered server:run_budget request never blocks done: its delivery needs
// a run the budget refuses.
func TestDoneGuardIgnoresAnsweredRunBudgetRequest(t *testing.T) {
	fixture := newAskedRecordFixture(t)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := completeAsAgent(t, fixture.agentID, fixture.issueID); err == nil {
		t.Fatal("control: a fresh answered request must still block done")
	}
	dbfx.Exec(t, `UPDATE decision_request SET request_fingerprint = 'server:run_budget:2026-11-09T00:00:00Z' WHERE id = $1`, recorded.Request.ID)
	if err := completeAsAgent(t, fixture.agentID, fixture.issueID); err != nil {
		t.Fatalf("done blocked by a server:run_budget request: %v", err)
	}
}

// Any answered request whose answer is over an hour old and still undelivered
// does not block done (CHE-2559).
func TestDoneGuardIgnoresStaleUndeliveredAnswer(t *testing.T) {
	fixture := newAskedRecordFixture(t)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE decision_answer SET created_at = now() - interval '61 minutes' WHERE id = $1`, recorded.Answer.ID)
	if err := completeAsAgent(t, fixture.agentID, fixture.issueID); err != nil {
		t.Fatalf("done blocked by a stale undelivered answer: %v", err)
	}
}

// moveWindow opens a new run-budget window the way a person's status move does.
func moveWindow(t *testing.T, issueID, userID string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details) VALUES ($1, $2, 'member', $3, 'status_changed', '{"trigger":{"cause":{"kind":"ui_or_api"},"auth_source":"interactive_jwt"}}')`,
		testWorkspaceID, issueID, userID)
}

// A card left behind by a window that has since reopened must not refuse a
// decision ask: it can no longer be approved, and nobody can withdraw it.
func TestDecisionAskIsNotRefusedByAStaleBudgetCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newBudgetRequestFixture(t)
	dbfx.Exec(t, `UPDATE issue SET requested_by_id = $1 WHERE id = $2`, fixture.humanID, fixture.issueID)
	exhaustBudget(t, fixture.agentID, fixture.issueID)
	fixture.request(t, fixture.humanID, "interactive_jwt", map[string]any{"kind": "top_up", "window_id": fixture.window}).Want(http.StatusCreated)
	moveWindow(t, fixture.issueID, fixture.humanID)
	if windowID(t, fixture.issueID) == fixture.window {
		t.Fatal("control: the status move did not open a new window")
	}
	for name, handler := range map[string]http.HandlerFunc{"ask": testHandler.AskDecision, "request": testHandler.RequestDecision} {
		t.Run(name, func(t *testing.T) {
			body := fixture.askBody()
			request := fixture.askCall(body)
			if name == "request" {
				body["issue_id"], body["for"] = fixture.issueID, fixture.humanID
				request = fixture.call(body)
			}
			if code := testutil.Call(t, handler, request).Code; code == http.StatusConflict {
				t.Fatalf("%s refused with 409 by a stale budget card", name)
			}
		})
	}
}

// A stale open card does not block an agent's done either.
func TestDoneGuardIgnoresAStaleBudgetCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newBudgetRequestFixture(t)
	dbfx.Exec(t, `UPDATE issue SET requested_by_id = $1 WHERE id = $2`, fixture.humanID, fixture.issueID)
	exhaustBudget(t, fixture.agentID, fixture.issueID)
	fixture.request(t, fixture.humanID, "interactive_jwt", map[string]any{"kind": "top_up", "window_id": fixture.window}).Want(http.StatusCreated)
	if err := completeAsAgent(t, fixture.agentID, fixture.issueID); err == nil {
		t.Fatal("control: a current budget card must block done")
	}
	moveWindow(t, fixture.issueID, fixture.humanID)
	if err := completeAsAgent(t, fixture.agentID, fixture.issueID); err != nil {
		t.Fatalf("done blocked by a stale budget card: %v", err)
	}
}

// Approving the card a refusal opened opens a new window, and no other card
// appears (CHE-5303: one refusal, one click).
func TestApprovingTheRefusalCardResumesTheIssueWithNoOtherCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2530303, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitNotices(t, "exhausted", 1)
	var requestID, actionID, humanID string
	dbfx.QueryRow(t, `SELECT r.id::text, a.id::text, r.human_user_id::text FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id WHERE r.issue_id = $1`, c.issueID).Scan(&requestID, &actionID, &humanID)
	dbfx.Exec(t, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, humanID)
	dbfx.Cleanup(t, `DELETE FROM decision_answer WHERE decision_request_id = $1`, requestID)
	before := windowID(t, c.issueID)

	card := pendingAction{singleCallFixture{intentFixture: intentFixture{issueID: c.issueID, humanID: humanID, agentID: c.agentID}}, requestID, actionID}
	card.approve(t, withVerifier(acceptingVerifier), card.claim(t, nil), http.StatusOK)

	if state := card.state(t); state != "executed" {
		t.Fatalf("card state = %s (%s), want executed", state, card.failureCode(t))
	}
	if after := windowID(t, c.issueID); after == before {
		t.Fatalf("window id still %s after approving the card", before)
	}
	if n := c.topUpCards(t); n != 0 {
		t.Fatalf("open top-up cards after approval = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND id <> $2`, c.issueID, requestID); n != 0 {
		t.Fatalf("other decision requests on the issue = %d, want 0", n)
	}
	if err := c.admit(t); err != nil {
		t.Fatalf("run refused after the top-up: %v", err)
	}
}
