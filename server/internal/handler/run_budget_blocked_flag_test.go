package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// budgetBlockedFlag reads run_budget_blocked for the issue from the detail
// and the list endpoints.
func (c budgetCase) budgetBlockedFlag(t *testing.T) (detail, list bool) {
	t.Helper()
	type payload struct {
		ID               string `json:"id"`
		RunBudgetBlocked bool   `json:"run_budget_blocked"`
	}
	rec := httptest.NewRecorder()
	testHandler.GetIssue(rec, withURLParam(newRequest("GET", "/api/issues/"+c.issueID, nil), "id", c.issueID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET issue = %d: %s", rec.Code, rec.Body.String())
	}
	var one payload
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	testHandler.ListIssues(rec, newRequest("GET", "/api/issues?status=blocked&limit=200", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list issues = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Issues []payload `json:"issues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Issues {
		if row.ID == c.issueID {
			list = row.RunBudgetBlocked
		}
	}
	return one.RunBudgetBlocked, list
}

// An issue the budget stopped carries run_budget_blocked; one a person moved
// to blocked, or one the budget stopped earlier, does not. (CHE-5285)
func TestRunBudgetBlockedFlag(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2528501, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitNotices(t, service.RunBudgetKindExhausted, 1)
	if got := c.status(t); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
	if detail, list := c.budgetBlockedFlag(t); !detail || !list {
		t.Fatalf("budget-stopped issue: run_budget_blocked detail=%v list=%v, want true on both", detail, list)
	}

	// A person unblocks, then blocks again: the budget no longer holds it.
	for _, status := range []string{"in_progress", "blocked"} {
		dbfxExecStatus(t, c.issueID, status)
	}
	if detail, list := c.budgetBlockedFlag(t); detail || list {
		t.Fatalf("person-blocked issue: run_budget_blocked detail=%v list=%v, want false on both", detail, list)
	}
}

func dbfxExecStatus(t *testing.T, issueID, status string) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), `UPDATE issue SET status=$2 WHERE id=$1`, issueID, status); err != nil {
		t.Fatal(err)
	}
}
