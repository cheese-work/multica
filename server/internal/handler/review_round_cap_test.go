package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6192: the round cap counts entries into agent_review, so it also stops a
// loop whose reviewer leaves through Blocked or a wakeup, which records no
// CHANGES verdict. The entry that would start round 4 goes to need_decision.

var roundCapSteps = []map[string]any{
	{"status_key": "in_progress", "edges": []map[string]any{{"to": "agent_review", "guard": "agent_done"}}},
	{"status_key": "agent_review", "edges": []map[string]any{{"to": "in_progress", "guard": "agent_verdict(CHANGES)"}}},
	{"status_key": "need_decision"},
	{"status_key": "blocked"},
}

type roundCap struct {
	agentID, issueID, run string
}

func newRoundCap(t *testing.T) roundCap {
	t.Helper()
	createTestCustomStatus(t, "agent_review", "started")
	createTestCustomStatus(t, "need_decision", "started")
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	wf := createTestWorkflowSteps(t, "Round cap "+workflowTestSuffix(), roundCapSteps)
	projectID := createWorkflowTestProject(t, "Round cap project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "round cap", testutil.Cols{
		"project_id": projectID, "status": "in_progress", "assignee_type": "agent", "assignee_id": agentID,
	})
	run := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "workflow_step": "in_progress",
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
	return roundCap{agentID, issueID, run}
}

// activity inserts one earlier activity row, minutesAgo minutes old.
func (c roundCap) activity(t *testing.T, action, to string, minutesAgo int) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
		SELECT workspace_id, id, 'agent', $2, $3, jsonb_build_object('to', $4::text), now() - make_interval(mins => $5) FROM issue WHERE id = $1`,
		c.issueID, c.agentID, action, to, minutesAgo)
}

func (c roundCap) enterReview(t *testing.T) {
	t.Helper()
	agentUpdateIssue(t, c.issueID, c.agentID, c.run, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
}

func (c roundCap) status(t *testing.T) string {
	t.Helper()
	status, _, _ := issueStatusAndAssignee(t, c.issueID)
	return status
}

func TestRoundCap_FourthEntryIntoReviewGoesToNeedDecision(t *testing.T) {
	c := newRoundCap(t)
	for _, ago := range []int{300, 200, 100} {
		c.activity(t, "status_changed", "agent_review", ago)
	}
	c.enterReview(t)
	if got := c.status(t); got != "need_decision" {
		t.Fatalf("status after the 4th entry = %q, want need_decision", got)
	}
	var comment string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id = $1 AND type = 'system'`, c.issueID).Scan(&comment)
	if !strings.Contains(comment, "3 review rounds") {
		t.Fatalf("system comment %q does not name the round count", comment)
	}
	var capped int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_rounds_capped'`, c.issueID).Scan(&capped)
	if capped != 1 {
		t.Fatalf("review_rounds_capped rows = %d, want 1", capped)
	}
}

func TestRoundCap_ThirdEntryStillGoesToReview(t *testing.T) {
	c := newRoundCap(t)
	c.activity(t, "status_changed", "agent_review", 200)
	c.activity(t, "status_changed", "agent_review", 100)
	c.enterReview(t)
	if got := c.status(t); got != "agent_review" {
		t.Fatalf("status after the 3rd entry = %q, want agent_review", got)
	}
}

// A PASS or an earlier cap ends the count: rounds restart from zero.
func TestRoundCap_PassOrEarlierCapRestartsTheCount(t *testing.T) {
	for _, marker := range []string{"review_passed", "review_rounds_capped"} {
		t.Run(marker, func(t *testing.T) {
			c := newRoundCap(t)
			c.activity(t, "status_changed", "agent_review", 300)
			c.activity(t, "status_changed", "agent_review", 250)
			c.activity(t, "status_changed", "agent_review", 200)
			c.activity(t, marker, "", 100)
			c.enterReview(t)
			if got := c.status(t); got != "agent_review" {
				t.Fatalf("status = %q, want agent_review", got)
			}
		})
	}
}
