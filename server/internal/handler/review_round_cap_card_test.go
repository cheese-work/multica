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
	testHandler.ensureReviewRoundsCard(t.Context(), issue, roundsCap{rounds: 3})
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system' AND content LIKE '%could not open its decision card%'`, issueID); n != 1 {
		t.Fatalf("missing-card notices = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("decision requests = %d, want 0", n)
	}
}

// CHE-9483: a plan(...) issue that reaches the round limit gets no decision
// card. The server records the auto-Split as an agent decision and tells the
// need_decision handler to run the Split path; a non-plan issue keeps the card.
func TestRoundCap_PlanIssueAutoSplitsWithoutCard(t *testing.T) {
	issueID := dbfx.Issue(t, "plan(WE006): split the workstream", testutil.Cols{})
	issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.ensureReviewRoundsCard(t.Context(), issue, roundsCap{rounds: 3})
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("decision requests = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_rounds_auto_split' AND details->>'rounds' = '3'`, issueID); n != 1 {
		t.Fatalf("auto-split activity rows = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system' AND content LIKE '%auto-Split at round limit%' AND content LIKE '%3 review rounds%'`, issueID); n != 1 {
		t.Fatalf("auto-split notices = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%could not open its decision card%'`, issueID); n != 0 {
		t.Fatalf("missing-card notices = %d, want 0", n)
	}
}

func TestRoundCap_IsPlanIssue(t *testing.T) {
	for title, want := range map[string]bool{
		"plan(WE006): x": true, "Plan(x): y": true, "plan: x": true, "  plan(x): y": true,
		"feat(plan): x": false, "planning notes": false, "explan(x): y": false, "": false,
	} {
		if got := isPlanIssueTitle(title); got != want {
			t.Errorf("isPlanIssueTitle(%q) = %v, want %v", title, got, want)
		}
	}
}
