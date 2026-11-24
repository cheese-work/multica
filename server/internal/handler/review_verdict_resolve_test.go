package handler

import (
	"net/http"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-7549: when agent_verdict(PASS) and agent_verdict(CHANGES) lead to the same
// status, the findings decide the verdict, and the guard and the record agree.

var sameTargetSteps = []map[string]any{
	{"status_key": "in_progress", "edges": []map[string]any{{"to": "agent_review", "guard": "agent_done"}}},
	{"status_key": "agent_review", "edges": []map[string]any{
		{"to": "in_progress", "guard": "agent_verdict(PASS)"},
		{"to": "in_progress", "guard": "agent_verdict(CHANGES)"},
	}},
	{"status_key": "need_decision"},
	{"status_key": "blocked"},
}

func newSameTarget(t *testing.T) reviewLoop {
	t.Helper()
	return newReviewLoopSteps(t, sameTargetSteps)
}

// backToReview puts the issue back in agent_review for the reviewer's next
// verdict.
func (l reviewLoop) backToReview(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET status = 'agent_review' WHERE id = $1`, l.issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET workflow_step = 'agent_review' WHERE id = $1`, l.run)
}

func (l reviewLoop) count(t *testing.T, action string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = $2`, l.issueID, action)
}

// Tests 1, 3, 4 and 6: the verdict follows the findings, and what the guard let
// through is what the record stores.
func TestVerdictResolve_SameTargetExitFollowsTheFindings(t *testing.T) {
	cases := []struct {
		name     string
		findings []map[string]any
		action   string
	}{
		{"P1 is CHANGES", []map[string]any{finding("P2"), finding("P1")}, "review_changes_requested"},
		{"P0 is CHANGES", []map[string]any{finding("P0")}, "review_changes_requested"},
		{"no findings is PASS", nil, "review_passed"},
		{"P2 only is PASS", []map[string]any{finding("P2")}, "review_passed"},
		{"P2 and P3 is PASS", []map[string]any{finding("P2"), finding("P3")}, "review_passed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newSameTarget(t)
			l.verdict(t, "in_progress", tc.findings...).Want(http.StatusOK)
			if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_progress" {
				t.Fatalf("issue status = %q, want in_progress", status)
			}
			other := "review_passed"
			if tc.action == other {
				other = "review_changes_requested"
			}
			if got, none := l.count(t, tc.action), l.count(t, other); got != 1 || none != 0 {
				t.Fatalf("%s rows = %d, %s rows = %d; want 1 and 0", tc.action, got, other, none)
			}
			if tc.action == "review_changes_requested" {
				var prID string
				dbfx.QueryRow(t, `SELECT coalesce(details->>'pull_request_id', 'missing') FROM activity_log WHERE issue_id = $1 AND action = $2`, l.issueID, tc.action).Scan(&prID)
				if prID != "" {
					t.Fatalf("pull_request_id = %q, want empty for a review without a PR", prID)
				}
			}
		})
	}
}

// Test 2: without a PR, CHANGES 1 and 2 go back to the author and the 3rd
// opens the cap card.
func TestVerdictResolve_NoPRThirdChangesIsCapped(t *testing.T) {
	l := newSameTarget(t)
	for round := 1; round <= 2; round++ {
		l.backToReview(t)
		l.verdict(t, "in_progress", finding("P1")).Want(http.StatusOK)
		if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_progress" {
			t.Fatalf("status after CHANGES %d = %q, want in_progress", round, status)
		}
	}
	if n := l.count(t, "review_changes_requested"); n != 2 {
		t.Fatalf("recorded CHANGES rounds = %d, want 2", n)
	}
	l.backToReview(t)
	l.verdict(t, "in_progress", finding("P1")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "need_decision" {
		t.Fatalf("status after CHANGES 3 = %q, want need_decision", status)
	}
	if n := l.count(t, "review_rounds_capped"); n != 1 {
		t.Fatalf("review_rounds_capped rows = %d, want 1", n)
	}
	roundCap{l.agentID, l.issueID, l.run}.wantAutoSplit(t, 3)
}

// Test 3: a PASS restarts both counts.
func TestVerdictResolve_PassRestartsBothCounts(t *testing.T) {
	l := newSameTarget(t)
	l.priorChangesRounds(t, 2, "")
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		SELECT workspace_id, id, 'agent', $2, 'status_changed', jsonb_build_object('to', 'agent_review') FROM issue WHERE id = $1`, l.issueID, l.agentID)
	l.verdict(t, "in_progress").Want(http.StatusOK)
	q := db.New(testPool)
	changes, err := q.CountChangesRoundsForPR(t.Context(), db.CountChangesRoundsForPRParams{IssueID: parseUUID(l.issueID), PullRequestID: ""})
	if err != nil || changes != 0 {
		t.Fatalf("CHANGES rounds after PASS = %d (err %v), want 0", changes, err)
	}
	entries, err := q.CountReviewEntriesSinceLastVerdict(t.Context(), parseUUID(l.issueID))
	if err != nil || entries != 0 {
		t.Fatalf("review entries after PASS = %d (err %v), want 0", entries, err)
	}
}

// Test 5: rounds of a PR and rounds without a PR count apart.
func TestVerdictResolve_PRAndNoPRRoundsCountApart(t *testing.T) {
	l := newSameTarget(t)
	l.priorChangesRounds(t, 2, "")
	l.linkPR(t, "sha-with-pr")
	l.verdict(t, "in_progress", finding("P1")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_progress" {
		t.Fatalf("status = %q, want in_progress: 2 rounds without a PR, 1 on the PR", status)
	}
	if n := l.count(t, "review_rounds_capped"); n != 0 {
		t.Fatalf("review_rounds_capped rows = %d, want 0", n)
	}
}

// Test 7: three entries that leave through blocked count as three rounds, so the
// 4th entry goes to need_decision (the CHE-6192 backstop).
func TestVerdictResolve_ThreeEntriesLeavingThroughBlockedCapTheFourth(t *testing.T) {
	c := newRoundCap(t)
	for i := 0; i < 3; i++ {
		c.enterReview(t)
		if got := c.status(t); got != "agent_review" {
			t.Fatalf("status after entry %d = %q, want agent_review", i+1, got)
		}
		dbfx.Exec(t, `UPDATE issue SET status = 'blocked' WHERE id = $1`, c.issueID)
		dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, c.issueID)
		dbfx.Exec(t, `UPDATE agent_task_queue SET workflow_step = 'in_progress' WHERE id = $1`, c.run)
	}
	c.enterReview(t)
	if got := c.status(t); got != "need_decision" {
		t.Fatalf("status after the 4th entry = %q, want need_decision", got)
	}
	c.wantAutoSplit(t, 3)
}
