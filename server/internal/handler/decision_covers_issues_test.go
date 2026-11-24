package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4029: covers_issue_ids is stored with the ask, where the decision wait
// guard reads it, and must name issues of the workspace.
func TestDecisionRequestStoresCoveredIssues(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	covered := dbfx.Issue(t, "covered by the group card", nil)
	ask := fixture.newShapeAsk()
	ask["covers_issue_ids"] = []any{covered}
	testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, ask)).Want(http.StatusCreated)
	if got := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE ask_json -> 'covers_issue_ids' @> to_jsonb($1::text)`, covered); got != 1 {
		t.Fatalf("requests storing the covered issue = %d, want 1", got)
	}
}

func TestDecisionRequestRefusesUnknownCoveredIssue(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	ask := fixture.newShapeAsk()
	ask["covers_issue_ids"] = []any{"99999999-9999-4999-8999-999999999999"}
	res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, ask)).Want(http.StatusBadRequest)
	if !strings.Contains(res.Text(), "ask.covers_issue_ids[0] is not an issue of this workspace") {
		t.Errorf("body = %s", res.Text())
	}
	fixture.assertCounts(t, 0)
}
