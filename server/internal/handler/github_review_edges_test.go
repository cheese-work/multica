package handler

import (
	"net/http"
	"testing"
)

// GitHub review guards (CHE-2269): a mapped member's review on the issue's PR
// moves an issue sitting on a step with a pr.review_* edge.

var reviewEdgeSteps = []map[string]any{
	{"status_key": "backlog"},
	{"status_key": "in_progress"},
	{"status_key": "in_review", "edges": []map[string]any{
		{"to": "in_progress", "guard": "pr.review_changes_requested", "timeout": "72h", "on_timeout": "blocked"},
		{"to": "done", "guard": "pr.review_approved", "timeout": "72h", "on_timeout": "blocked"},
	}},
	{"status_key": "done"},
	{"status_key": "blocked"},
}

// newReviewEdgeCase puts the issue that carries the PR on in_review of a
// workflow with reviewEdgeSteps.
func newReviewEdgeCase(t *testing.T) (githubReviewFixture, edgeCase) {
	t.Helper()
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	fixture := newGitHubIntakeFixture(t)
	wf := createTestWorkflowSteps(t, "Review edges "+workflowTestSuffix(), reviewEdgeSteps)
	projectID := createWorkflowTestProject(t, "Review edges project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue SET project_id=$2, status='backlog', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, fixture.issueID, projectID)
	c := edgeCase{workflowID: wf.ID, issueID: fixture.issueID}
	c.enter(t, "in_review")
	return fixture, c
}

func (fixture githubReviewFixture) review(t *testing.T, id int64, state string) {
	t.Helper()
	review := fixture.payload["review"].(map[string]any)
	review["id"], review["state"] = id, state
	if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+state); response.Code != http.StatusAccepted {
		t.Fatalf("%s review: %d %s", state, response.Code, response.Body.String())
	}
}

// reviewAt delivers a submitted review with its own GitHub submit time and
// delivery id, so a late redelivery of an older review can be replayed.
func (fixture githubReviewFixture) reviewAt(t *testing.T, id int64, state, submittedAt, delivery string) {
	t.Helper()
	review := fixture.payload["review"].(map[string]any)
	review["id"], review["state"], review["submitted_at"] = id, state, submittedAt
	fixture.payload["action"] = "submitted"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		t.Fatalf("%s review: %d %s", state, response.Code, response.Body.String())
	}
}

func (fixture githubReviewFixture) dismiss(t *testing.T, id int64) {
	t.Helper()
	review := fixture.payload["review"].(map[string]any)
	review["id"], review["state"] = id, "dismissed"
	fixture.payload["action"] = "dismissed"
	if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"dismissed"); response.Code != http.StatusAccepted {
		t.Fatalf("dismissed review: %d %s", response.Code, response.Body.String())
	}
}

func TestEdgesGitHubReviewMovesIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := []struct{ name, state, want string }{
		{"changes requested", "changes_requested", "in_progress"},
		{"approved", "approved", "done"},
		{"commented", "commented", "in_review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, c := newReviewEdgeCase(t)
			fixture.review(t, 77, tc.state)
			tickEdges(t)
			if got := c.status(t); got != tc.want {
				t.Fatalf("%s review left the issue at %s, want %s", tc.state, got, tc.want)
			}
		})
	}
}

// A review of an older head does not count once the author pushed again.
func TestEdgesGitHubReviewOfOldHeadIsIgnored(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture, c := newReviewEdgeCase(t)
	fixture.review(t, 77, "changes_requested")
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha='newer-head' WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "in_review" {
		t.Fatalf("review of an old head moved the issue to %s", got)
	}
}

// A later comment-only review on the same head supersedes an earlier decision,
// so the stale decision cannot fire a guard.
func TestEdgesGitHubLaterCommentClearsEarlierDecision(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture, c := newReviewEdgeCase(t)
	fixture.review(t, 77, "changes_requested")
	fixture.review(t, 78, "commented")
	tickEdges(t)
	if got := c.status(t); got != "in_review" {
		t.Fatalf("changes_requested then commented on the same head moved the issue to %s", got)
	}
}

// Dismissing the review that holds the stored decision withdraws it; dismissing
// any other review leaves it alone.
func TestEdgesGitHubDismissedReview(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name      string
		dismissed int64
		want      string
	}{
		{"the deciding review", 77, "in_review"},
		{"an older review", 70, "in_progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, c := newReviewEdgeCase(t)
			fixture.review(t, 77, "changes_requested")
			fixture.dismiss(t, tc.dismissed)
			tickEdges(t)
			if got := c.status(t); got != tc.want {
				t.Fatalf("dismissing review %d left the issue at %s, want %s", tc.dismissed, got, tc.want)
			}
		})
	}
}

// Dismissal keeps the ordering watermark: an older review that arrives late
// after the deciding review was dismissed cannot bring its decision back.
func TestEdgesGitHubLateOlderReviewAfterDismissalIsIgnored(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture, c := newReviewEdgeCase(t)
	fixture.reviewAt(t, 70, "changes_requested", "2026-10-26T08:01:00Z", fixture.issueID+"a")
	fixture.reviewAt(t, 71, "commented", "2026-10-26T08:02:00Z", fixture.issueID+"b")
	fixture.dismiss(t, 71)
	fixture.reviewAt(t, 70, "changes_requested", "2026-10-26T08:01:00Z", fixture.issueID+"a-late")
	tickEdges(t)
	if got := c.status(t); got != "in_review" {
		t.Fatalf("a late older review after the dismissal moved the issue to %s", got)
	}
}
