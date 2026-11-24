package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// When no card can open (a workspace with nobody to ask), the issue says so in
// a system comment: need_decision never lacks its card silently.
func TestRoundCap_MissingCardIsAnnouncedOnTheIssue(t *testing.T) {
	suffix := workflowTestSuffix()
	wsID := dbfx.Workspace(t, "Empty "+suffix, "empty-"+suffix)
	issueID := dbfx.Issue(t, "no one to ask", testutil.Cols{"workspace_id": wsID})
	issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	roundCap{issueID: issueID}.failAutoSplit(t)
	testHandler.ensureReviewRoundsCard(t.Context(), issue, roundsCap{rounds: 3})
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system' AND content LIKE '%could not open its decision card%'`, issueID); n != 1 {
		t.Fatalf("missing-card notices = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("decision requests = %d, want 0", n)
	}
}

// CHE-9483, CHE-9511: an issue of any type that reaches the round limit gets no
// decision card. The server records the auto-Split as an agent decision and
// tells the need_decision handler to run the Split path.
func TestRoundCap_AnyIssueTypeAutoSplitsWithoutCard(t *testing.T) {
	for _, title := range []string{"plan(WE006): split the workstream", "fix(handler): split the fix", "task: split the task"} {
		issueID := dbfx.Issue(t, title, testutil.Cols{})
		issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(issueID))
		if err != nil {
			t.Fatal(err)
		}
		testHandler.ensureReviewRoundsCard(t.Context(), issue, roundsCap{rounds: 3})
		roundCap{issueID: issueID}.wantAutoSplit(t, 3)
		if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%could not open its decision card%'`, issueID); n != 0 {
			t.Fatalf("%s: missing-card notices = %d, want 0", title, n)
		}
	}
}
