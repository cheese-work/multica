package handler

import (
	"github.com/multica-ai/multica/server/internal/testutil"
	"net/http"
	"testing"
)

// CHE-9114: a same_as step hands an issue to the agent that last handled the
// step it names, on this issue. Fallback: the step's own agent id, else the
// named step's configured handler.

type sameAsCase struct {
	projectID string
	reviewer  string // handled in_review on the issue
	configd   string // in_review's configured handler
	fixed     string // merged's own fallback agent
}

func newSameAsCase(t *testing.T, mergedFallback bool) sameAsCase {
	t.Helper()
	createTestCustomStatus(t, "merged", "started")
	first := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, first).Scan(&runtimeID)
	c := sameAsCase{
		reviewer: dbfx.Agent(t, "SameAs reviewer "+workflowTestSuffix(), runtimeID),
		configd:  first,
		fixed:    dbfx.Agent(t, "SameAs fixed "+workflowTestSuffix(), runtimeID),
	}
	c.projectID = createWorkflowTestProject(t, "SameAs project "+workflowTestSuffix())
	setColumnHandoff(t, c.projectID, "in_review", handToAgent(c.configd, "Review it.")).Want(http.StatusOK)
	handler := map[string]any{"type": "same_as", "step": "in_review"}
	if mergedFallback {
		handler["id"] = c.fixed
	}
	setColumnHandoff(t, c.projectID, "merged", map[string]any{"handler": handler, "instructions": "Land it."}).Want(http.StatusOK)
	return c
}

func (c sameAsCase) issue(t *testing.T) string {
	t.Helper()
	return dbfx.Issue(t, "feat(x): same_as", testutil.Cols{"project_id": c.projectID, "status": "in_review"})
}

func (c sameAsCase) assign(t *testing.T, issueID, agentID string) {
	t.Helper()
	updateIssueForTest(t, issueID, map[string]any{"assignee_type": "agent", "assignee_id": agentID}).Want(http.StatusOK)
}

func TestSameAs_StepTakesTheAgentThatHandledTheNamedStep(t *testing.T) {
	c := newSameAsCase(t, true)
	issueID := c.issue(t)
	c.assign(t, issueID, c.reviewer)

	updateIssueForTest(t, issueID, map[string]any{"status": "merged"}).Want(http.StatusOK)

	if status, typ, id := issueStatusAndAssignee(t, issueID); status != "merged" || typ != "agent" || id != c.reviewer {
		t.Fatalf("issue = (%s, %s, %s), want merged assigned to reviewer %s, not the fixed agent", status, typ, id, c.reviewer)
	}
}

func TestSameAs_RemembersTheAgentAcrossIntermediateSteps(t *testing.T) {
	c := newSameAsCase(t, true)
	issueID := c.issue(t)
	c.assign(t, issueID, c.reviewer)
	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	c.assign(t, issueID, c.fixed)

	updateIssueForTest(t, issueID, map[string]any{"status": "merged"}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != c.reviewer {
		t.Fatalf("assignee = %s, want reviewer %s, who last handled in_review", id, c.reviewer)
	}
}

func TestSameAs_NamedStepNeverVisitedFallsBackToTheStepsOwnAgent(t *testing.T) {
	c := newSameAsCase(t, true)
	issueID := dbfx.Issue(t, "feat(x): same_as", testutil.Cols{"project_id": c.projectID, "status": "todo"})

	updateIssueForTest(t, issueID, map[string]any{"status": "merged"}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != c.fixed {
		t.Fatalf("assignee = %s, want the step's own agent %s", id, c.fixed)
	}
}

func TestSameAs_NamedStepNeverVisitedWithoutOwnAgentUsesTheNamedStepsHandler(t *testing.T) {
	c := newSameAsCase(t, false)
	issueID := dbfx.Issue(t, "feat(x): same_as", testutil.Cols{"project_id": c.projectID, "status": "todo"})

	updateIssueForTest(t, issueID, map[string]any{"status": "merged"}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != c.configd {
		t.Fatalf("assignee = %s, want in_review's configured handler %s", id, c.configd)
	}
}

func TestSameAs_ArchivedAgentFallsBack(t *testing.T) {
	c := newSameAsCase(t, true)
	issueID := c.issue(t)
	c.assign(t, issueID, c.reviewer)
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, c.reviewer)

	updateIssueForTest(t, issueID, map[string]any{"status": "merged"}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != c.fixed {
		t.Fatalf("assignee = %s, want the fallback agent %s once the reviewer is archived", id, c.fixed)
	}
}

func TestSameAs_SaveRefusesAStepOfAnotherWorkflowAndACycle(t *testing.T) {
	c := newSameAsCase(t, false)
	for name, step := range map[string]string{"unknown step": "nowhere", "itself": "merged"} {
		resp := setColumnHandoff(t, c.projectID, "merged", map[string]any{"handler": map[string]any{"type": "same_as", "step": step}})
		if resp.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.Code)
		}
	}
	// merged already follows in_review, so in_review cannot follow merged.
	resp := setColumnHandoff(t, c.projectID, "in_review", map[string]any{"handler": map[string]any{"type": "same_as", "step": "merged"}})
	if resp.Code != http.StatusBadRequest {
		t.Errorf("cycle: status %d, want 400", resp.Code)
	}
}

// RGS-1 still holds when a same_as step is agent_review: the resolved agent is
// held to the reviewer independence rule, as a reviewer step's pick is.
func TestSameAs_AgentReviewHoldsTheResolvedAgentToIndependence(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	sameLab := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	otherLab := reviewerAgent(t, "codex", "gpt-5.5")
	seedTestCatalog(t)
	createTestCustomStatus(t, "agent_review", "started")
	projectID := createWorkflowTestProject(t, "SameAs review project "+workflowTestSuffix())
	setColumnHandoff(t, projectID, "agent_review", map[string]any{
		"handler": map[string]any{"type": "same_as", "step": "in_review"}, "instructions": "Review it.",
	}).Want(http.StatusOK)
	newIssue := func(t *testing.T, handledInReview string) string {
		t.Helper()
		issueID := dbfx.Issue(t, "feat(x): same_as review "+workflowTestSuffix(), testutil.Cols{"project_id": projectID, "status": "in_review"})
		var runtimeID string
		dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, author).Scan(&runtimeID)
		dbfx.Task(t, author, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "applied_model": "claude-sonnet-5-5"})
		updateIssueForTest(t, issueID, map[string]any{"assignee_type": "agent", "assignee_id": handledInReview}).Want(http.StatusOK)
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled' WHERE issue_id = $1 AND status = 'queued'`, issueID)
		return issueID
	}

	t.Run("the author is refused", func(t *testing.T) {
		issueID := newIssue(t, author)
		wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "agent_review"}), issueID, "in_review")
	})
	t.Run("a same-lab agent is refused", func(t *testing.T) {
		issueID := newIssue(t, sameLab)
		wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "agent_review"}), issueID, "in_review", "anthropic")
	})
	t.Run("an agent of another lab is accepted", func(t *testing.T) {
		issueID := newIssue(t, otherLab)
		updateIssueForTest(t, issueID, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
		if _, _, id := issueStatusAndAssignee(t, issueID); id != otherLab {
			t.Fatalf("assignee = %s, want the other-lab agent %s", id, otherLab)
		}
	})
}

// A same_as agent_review step that finds nobody keeps the issue's assignee;
// when that is the author, the move is refused rather than leaving the author
// on the review.
func TestSameAs_AgentReviewWithNoResolutionRefusesAKeptAuthor(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	seedTestCatalog(t)
	createTestCustomStatus(t, "agent_review", "started")
	projectID := createWorkflowTestProject(t, "SameAs unresolved project "+workflowTestSuffix())
	setColumnHandoff(t, projectID, "in_review", map[string]any{"handler": map[string]any{"type": "none"}}).Want(http.StatusOK)
	setColumnHandoff(t, projectID, "agent_review", map[string]any{
		"handler": map[string]any{"type": "same_as", "step": "in_review"}, "instructions": "Review it.",
	}).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "feat(x): same_as unresolved "+workflowTestSuffix(), testutil.Cols{"project_id": projectID, "status": "in_progress"})
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, author).Scan(&runtimeID)
	dbfx.Task(t, author, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "applied_model": "claude-sonnet-5-5"})
	updateIssueForTest(t, issueID, map[string]any{"assignee_type": "agent", "assignee_id": author}).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled' WHERE issue_id = $1 AND status = 'queued'`, issueID)

	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "agent_review"}), issueID, "in_progress")
}
