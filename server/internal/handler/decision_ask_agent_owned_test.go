package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// An ask whose human step is agent-owned is refused with a field-specific 400
// and writes nothing: no request, ask comment, DM, ping or inbox notification.
func TestDecisionAskRefusesAgentOwnedStepsWithoutWrites(test *testing.T) {
	actions := map[string]string{
		"merge without a destination":    "Merge PR #7",
		"second merge to another branch": "Merge PR #7 into the default branch; merge PR #8 into develop",
		"retarget by short flag":         "Run gh pr edit 7 -B develop",
		"parenthetical default branch":   "Merge PR #7 (originally meant to go into the default branch) into develop",
		"mark the PR ready":              "Mark PR #7 ready for review",
	}
	for name, action := range actions {
		for _, field := range []string{"action", "command"} {
			test.Run(name+"/"+field, func(test *testing.T) {
				fixture := newRequestCoreFixture(test, false)
				step := map[string]any{"action": "Look at the result", "verify_done": "PR shows Merged"}
				step[field] = action
				ask := fixture.newShapeAsk()
				ask["human_steps"] = []any{step}
				ask["probes"] = []string{"not verified: nothing probed"}
				ask["after_done"] = "x99-claude-sonnet starts the next slice"
				comments := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID)

				response := testutil.Call(test, testHandler.RequestDecision, fixture.askRequest(test, ask)).Want(http.StatusBadRequest)

				want := "ask.human_steps[0]." + field + " is an agent-owned step"
				if got := response.Body.String(); !strings.Contains(got, want) {
					test.Fatalf("body = %s, want it to contain %q", got, want)
				}
				fixture.assertCounts(test, 0)
				if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); count != 0 {
					test.Fatalf("refusal created %d inbox items", count)
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID); count != comments {
					test.Fatalf("refusal changed comments from %d to %d", comments, count)
				}
			})
		}
	}
}
