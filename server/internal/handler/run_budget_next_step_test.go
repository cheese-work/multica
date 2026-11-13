package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5503: a refused run, a budget read and a repeat ask all say what to do next.

type budgetCardView struct {
	RequestID string `json:"request_id"`
	ActionID  string `json:"action_id"`
	URL       string `json:"url"`
}

type budgetNextStep struct {
	Code        string          `json:"code"`
	ReasonCode  string          `json:"reason_code"`
	RunsUsed    int             `json:"runs_used"`
	RunsLimit   int             `json:"runs_limit"`
	WorkflowID  string          `json:"workflow_id"`
	WindowID    string          `json:"window_id"`
	OpenCard    *budgetCardView `json:"open_card"`
	NextCommand string          `json:"next_command"`
}

func (c budgetCase) identifier(t *testing.T) string {
	t.Helper()
	var number int
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, c.issueID).Scan(&number)
	return fmt.Sprintf("%s-%d", testHandler.getIssuePrefix(context.Background(), parseUUID(testWorkspaceID)), number)
}

func (c budgetCase) get(t *testing.T) budgetNextStep {
	t.Helper()
	var got budgetNextStep
	request := withURLParam(newRequest(http.MethodGet, "/api/issues/"+c.issueID+"/run-budget", nil), "id", c.issueID)
	testutil.Call(t, testHandler.GetIssueRunBudget, request).Want(http.StatusOK).JSON(&got)
	return got
}

func (c budgetCase) noticeText(t *testing.T, kind string) string {
	t.Helper()
	var content string
	dbfx.QueryRow(t, `SELECT c.content FROM run_budget_notice n JOIN comment c ON c.id=n.comment_id WHERE n.issue_id=$1 AND n.kind=$2`, c.issueID, kind).Scan(&content)
	return content
}

func TestRunBudgetRefusedRerunReturnsTypedNextStep(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310031, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitNotices(t, service.RunBudgetKindExhausted, 1)
	request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+c.issueID+"/rerun", map[string]any{"reason": "again"}), "id", c.issueID)
	recorder := httptest.NewRecorder()
	testHandler.RerunIssue(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("rerun = %d: %s", recorder.Code, recorder.Body.String())
	}
	var got budgetNextStep
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != "run_budget_exhausted" || got.RunsUsed != 2 || got.RunsLimit != 2 || got.WorkflowID != c.workflowID || got.WindowID == "" {
		t.Fatalf("typed refusal = %+v", got)
	}
	if got.OpenCard == nil || got.OpenCard.URL == "" || !strings.Contains(got.NextCommand, got.OpenCard.URL) || !strings.Contains(strings.ToLower(got.NextCommand), "do not open another") {
		t.Fatalf("open card / next command = %+v / %q", got.OpenCard, got.NextCommand)
	}
}

func TestRunBudgetGetNamesTheNextCommandAndTheOpenCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310032, mergeEdgeSteps, 2)
	c.spend(t, 1)
	before := c.get(t)
	if want := "multica issue budget raise " + c.identifier(t) + " --runs "; before.OpenCard != nil || !strings.HasPrefix(before.NextCommand, want) {
		t.Fatalf("budget without a card = %+v, want next_command starting %q", before, want)
	}
	c.spend(t, 1)
	c.refuse(t)
	c.waitNotices(t, service.RunBudgetKindExhausted, 1)
	after := c.get(t)
	if after.OpenCard == nil || after.OpenCard.RequestID == "" || !strings.Contains(after.NextCommand, after.OpenCard.URL) {
		t.Fatalf("budget with a card = %+v", after)
	}
}

func TestRunBudgetWarn80NoticeCarriesNumbersAndTheRaiseCommand(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310033, mergeEdgeSteps, 5)
	c.spend(t, 4)
	c.waitNotices(t, service.RunBudgetKindWarn80, 1)
	text := c.noticeText(t, service.RunBudgetKindWarn80)
	ident := c.identifier(t)
	for _, want := range []string{"Run budget: 4 of 5 runs used on " + ident + " (workflow ", ", cap 5). Ask for more: multica issue budget raise " + ident + " --runs "} {
		if !strings.Contains(text, want) {
			t.Fatalf("warn80 notice = %q, want it to contain %q", text, want)
		}
	}
}

func TestRepeatRaiseWhileACardIsOpenReturnsThatCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	useProposalGuard(t)
	fixture, window := budgetedRequestFixture(t)
	body := func(newCap int) map[string]any {
		return map[string]any{"issue_id": fixture.issueID, "action_type": raiseAction, "version": 1, "for": fixture.humanID, "params": fixture.raiseParams(window, newCap)}
	}
	first := fixture.propose(t, body(baseCap+2), http.StatusCreated)
	again := fixture.propose(t, body(baseCap+5), http.StatusOK)
	if again.ID != first.ID {
		t.Fatalf("repeat raise opened %s, want the open card %s", again.ID, first.ID)
	}
	var open struct {
		OpenCard *budgetCardView `json:"open_card"`
	}
	request := withURLParam(newRequest(http.MethodGet, "/api/issues/"+fixture.issueID+"/run-budget", nil), "id", fixture.issueID)
	testutil.Call(t, testHandler.GetIssueRunBudget, request).Want(http.StatusOK).JSON(&open)
	if open.OpenCard == nil || open.OpenCard.RequestID != first.ID {
		t.Fatalf("budget get open_card = %+v, want request %s", open.OpenCard, first.ID)
	}
}

func TestRepeatRaiseDefaultsTheRecipientToAWorkspaceOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	useProposalGuard(t)
	fixture, window := budgetedRequestFixture(t)
	body := map[string]any{"issue_id": fixture.issueID, "action_type": raiseAction, "version": 1, "params": fixture.raiseParams(window, baseCap+2)}
	got := fixture.propose(t, body, http.StatusCreated)
	var owners int
	dbfx.QueryRow(t, `SELECT count(*) FROM member WHERE workspace_id = $1 AND user_id = $2 AND role = 'owner'`, testWorkspaceID, got.HumanUserID).Scan(&owners)
	if owners != 1 {
		t.Fatalf("recipient %s is not a workspace owner", got.HumanUserID)
	}
}

// One run_budget_ask row per opened ask, by who opened it (CHE-5236 M2).
func TestRunBudgetAskRowsNameTheOrigin(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ask := func(t *testing.T, issueID string) (actorType, actorID, kind string) {
		t.Helper()
		if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'run_budget_ask'`, issueID); n != 1 {
			t.Fatalf("run_budget_ask rows = %d, want 1", n)
		}
		dbfx.QueryRow(t, `SELECT actor_type, COALESCE(actor_id::text, ''), details->>'action_type' FROM activity_log WHERE issue_id = $1 AND action = 'run_budget_ask'`, issueID).Scan(&actorType, &actorID, &kind)
		return actorType, actorID, kind
	}
	t.Run("member", func(t *testing.T) {
		fixture := newBudgetRequestFixture(t)
		dbfx.Cleanup(t, `DELETE FROM activity_log WHERE issue_id = $1`, fixture.issueID)
		dbfx.Exec(t, `INSERT INTO run_budget_refusal (id, workspace_id, issue_id, window_id) VALUES ($1, $2, $3, $4)`, uuid.NewString(), testWorkspaceID, fixture.issueID, fixture.window)
		fixture.request(t, fixture.humanID, "pat", map[string]any{"kind": "raise", "new_cap": 9, "window_id": fixture.window}).Want(http.StatusCreated)
		if actorType, actorID, kind := ask(t, fixture.issueID); actorType != "member" || actorID != fixture.humanID || kind != raiseAction {
			t.Fatalf("member ask row = %s %s %s", actorType, actorID, kind)
		}
	})
	t.Run("agent", func(t *testing.T) {
		useProposalGuard(t)
		fixture, window := budgetedRequestFixture(t)
		body := map[string]any{"issue_id": fixture.issueID, "action_type": raiseAction, "version": 1, "for": fixture.humanID, "params": fixture.raiseParams(window, baseCap+2)}
		fixture.propose(t, body, http.StatusCreated)
		fixture.propose(t, body, http.StatusOK) // a replay writes no second row
		if actorType, actorID, kind := ask(t, fixture.issueID); actorType != "agent" || actorID != fixture.agentID || kind != raiseAction {
			t.Fatalf("agent ask row = %s %s %s", actorType, actorID, kind)
		}
	})
	t.Run("system", func(t *testing.T) {
		c := newBudgetCase(t, 2310035, mergeEdgeSteps, 2)
		c.spend(t, 2)
		c.refuse(t)
		c.waitNotices(t, service.RunBudgetKindExhausted, 1)
		if actorType, _, kind := ask(t, c.issueID); actorType != "system" || kind != topUpAction {
			t.Fatalf("system ask row = %s %s", actorType, kind)
		}
	})
}
