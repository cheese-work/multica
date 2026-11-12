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

func completeAsAgent(t *testing.T, fixture askedRecordFixture) error {
	t.Helper()
	ctx := context.WithValue(context.Background(), wakeupActorKey{}, wakeupActor{kind: "agent", id: fixture.agentID})
	return guardAgentIssueStatus(ctx, testHandler.Queries, db.UpdateIssueParams{ID: parseUUID(fixture.issueID), Status: pgtype.Text{String: "done", Valid: true}})
}

// An answered server:run_budget request never blocks done: its delivery needs
// a run the budget refuses.
func TestDoneGuardIgnoresAnsweredRunBudgetRequest(t *testing.T) {
	fixture := newAskedRecordFixture(t)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := completeAsAgent(t, fixture); err == nil {
		t.Fatal("control: a fresh answered request must still block done")
	}
	dbfx.Exec(t, `UPDATE decision_request SET request_fingerprint = 'server:run_budget:2026-11-09T00:00:00Z' WHERE id = $1`, recorded.Request.ID)
	if err := completeAsAgent(t, fixture); err != nil {
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
	if err := completeAsAgent(t, fixture); err != nil {
		t.Fatalf("done blocked by a stale undelivered answer: %v", err)
	}
}
