package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// recordMemberMovesOf logs a person's status move on the issue the way the
// activity listener in cmd/server does, before the handler claims the window.
func recordMemberMovesOf(t *testing.T, issueID string) {
	t.Helper()
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, _ := e.Payload.(map[string]any)
		issue, _ := payload["issue"].(IssueResponse)
		if changed, _ := payload["status_changed"].(bool); !changed || issue.ID != issueID {
			return
		}
		if _, err := testHandler.Queries.CreateActivity(context.Background(), db.CreateActivityParams{
			WorkspaceID: parseUUID(issue.WorkspaceID), IssueID: parseUUID(issue.ID),
			ActorType: strToText(e.ActorType), ActorID: parseUUID(e.ActorID), Action: "status_changed", Details: []byte("{}"),
		}); err != nil {
			t.Error(err)
		}
	})
}

func moveStatusAsMember(t *testing.T, issueID, status string) {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"status": status, "suppress_run": true}), "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

func runBudgetSnapshot(t *testing.T, issueID string) service.RunBudgetSnapshot {
	t.Helper()
	req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID+"/run-budget", nil), "id", issueID)
	w := httptest.NewRecorder()
	testHandler.GetIssueRunBudget(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get run budget = %d: %s", w.Code, w.Body.String())
	}
	var snap service.RunBudgetSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

// A person's status move credits its window's allowance when the move is
// recorded: the budget read before any admission or report shows it.
func TestRunBudgetMemberStatusMoveClaimsItsAllowanceBeforeAnyAdmission(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	storeTokenValues(t, c)
	admitRun(t, c) // activates
	finishRuns(t, c)
	recordMemberMovesOf(t, c.issueID)
	before := runBudgetSnapshot(t, c.issueID)

	moveStatusAsMember(t, c.issueID, "in_review")

	if n := entryCount(t, c.issueID, "allowance"); n != 2 {
		t.Fatalf("allowance entries after one move = %d, want 2 (activation window, move window)", n)
	}
	after := runBudgetSnapshot(t, c.issueID)
	if after.TokensBalance != before.TokensBalance+actAllowance {
		t.Fatalf("tokens balance after the move = %d, want %d", after.TokensBalance, before.TokensBalance+actAllowance)
	}
}
