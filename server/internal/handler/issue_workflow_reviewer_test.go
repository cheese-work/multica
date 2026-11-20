package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7678 (step 13 of CHE-7658): the reviewer handler routes a step to the
// first reachable reviewer of the issue type's capability_routing entry whose
// lab is known and differs from every author lab. The write is refused with
// 409 review_independence, and the issue left unchanged, when none qualifies
// or when the effective assignee (override or sticky) is not independent.

const reviewerTestLabs = `{"claude/claude-sonnet-5-5":"anthropic","codex/gpt-5.5":"openai","gemini/gemini-3":"google"}`

type reviewerCase struct {
	projectID string
}

// reviewerAgent is a ready agent on its own runtime of the given provider and
// model. Its lab comes from MULTICA_VOTE_GATE_LABS through that pair.
func reviewerAgent(t *testing.T, provider, model string) string {
	t.Helper()
	runtimeID := dbfx.Runtime(t, "reviewer runtime "+workflowTestSuffix(), testutil.Cols{"provider": provider})
	return dbfx.Agent(t, "reviewer agent "+workflowTestSuffix(), runtimeID, testutil.Cols{"model": model, "visibility": "workspace"})
}

// newReviewerCase is a project whose workflow hands in_review to the reviewer
// handler, with capability_routing for type feat naming reviewers in order.
func newReviewerCase(t *testing.T, authorAgentID string, reviewers ...string) reviewerCase {
	t.Helper()
	seedTestCatalog(t)
	t.Setenv("MULTICA_VOTE_GATE_LABS", reviewerTestLabs)
	setWorkspaceSettings(t, mustJSON(t, map[string]any{
		"capability_routing": map[string]any{"feat": map[string]any{"author": authorAgentID, "reviewers": reviewers}},
	}))
	projectID := createWorkflowTestProject(t, "Reviewer handler project "+workflowTestSuffix())
	setColumnHandoff(t, projectID, "in_review", map[string]any{
		"handler": map[string]any{"type": "reviewer"}, "instructions": "Review the PR.",
	}).Want(http.StatusOK)
	return reviewerCase{projectID: projectID}
}

// issue is an in_progress feat issue on which each author agent has run.
func (c reviewerCase) issue(t *testing.T, authors ...string) string {
	t.Helper()
	issueID := dbfx.Issue(t, "feat(workflow): reviewer routing "+workflowTestSuffix(), testutil.Cols{"project_id": c.projectID, "status": "in_progress"})
	for _, agentID := range authors {
		var runtimeID, model string
		dbfx.QueryRow(t, `SELECT runtime_id::text, COALESCE(model, '') FROM agent WHERE id = $1`, agentID).Scan(&runtimeID, &model)
		dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "applied_model": model})
	}
	return issueID
}

func wantReviewerRefusal(t *testing.T, resp *testutil.Response, issueID, wantStatus string, wantInBody ...string) {
	t.Helper()
	resp.Want(http.StatusConflict)
	body := resp.Body.String()
	if !strings.Contains(body, `"review_independence"`) {
		t.Fatalf("body = %s, want code review_independence", body)
	}
	for _, s := range wantInBody {
		if !strings.Contains(body, s) {
			t.Fatalf("body = %s, want it to name %q", body, s)
		}
	}
	if status, _, _ := issueStatusAndAssignee(t, issueID); status != wantStatus {
		t.Fatalf("status = %q after the refusal, want the issue unchanged at %q", status, wantStatus)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID); n != 0 {
		t.Fatalf("queued runs = %d after the refusal, want 0", n)
	}
}

// An Anthropic author gets the first reviewer of another lab: the same-lab
// reviewer listed first is skipped.
func TestReviewerHandler_AnthropicAuthorGetsOpenAIReviewer(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	sameLab := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, author, sameLab, openai)
	issueID := c.issue(t, author)

	updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)

	if status, typ, id := issueStatusAndAssignee(t, issueID); status != "in_review" || typ != "agent" || id != openai {
		t.Fatalf("issue = (%s, %s, %s), want in_review assigned to the OpenAI reviewer %s", status, typ, id, openai)
	}
}

// With authors from two labs the reviewer comes from a third lab; when the
// table lists none, the move is refused and names the labs.
func TestReviewerHandler_TwoAuthorLabsNeedAThirdOrRefuse(t *testing.T) {
	anthropicAuthor := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openaiAuthor := reviewerAgent(t, "codex", "gpt-5.5")
	google := reviewerAgent(t, "gemini", "gemini-3")
	anthropicReviewer := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openaiReviewer := reviewerAgent(t, "codex", "gpt-5.5")

	t.Run("a third lab is chosen", func(t *testing.T) {
		c := newReviewerCase(t, anthropicAuthor, anthropicReviewer, openaiReviewer, google)
		issueID := c.issue(t, anthropicAuthor, openaiAuthor)
		updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)
		if _, _, id := issueStatusAndAssignee(t, issueID); id != google {
			t.Fatalf("assignee = %s, want the third-lab reviewer %s", id, google)
		}
	})
	t.Run("no third lab refuses", func(t *testing.T) {
		c := newReviewerCase(t, anthropicAuthor, anthropicReviewer, openaiReviewer)
		issueID := c.issue(t, anthropicAuthor, openaiAuthor)
		wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "anthropic", "openai")
	})
}

// A named assignee is held to the same rule as the table's pick.
func TestReviewerHandler_SameLabOverrideRefused(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	sameLab := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	c := newReviewerCase(t, author, openai)
	issueID := c.issue(t, author)

	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{
		"status": "in_review", "handoff_assignee": overrideTo(sameLab),
	}), issueID, "in_progress", "anthropic")

	// The author itself is refused the same way.
	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{
		"status": "in_review", "handoff_assignee": overrideTo(author),
	}), issueID, "in_progress")

	// A cross-lab override passes.
	other := reviewerAgent(t, "gemini", "gemini-3")
	updateIssueForTest(t, issueID, map[string]any{
		"status": "in_review", "handoff_assignee": overrideTo(other),
	}).Want(http.StatusOK)
	if _, _, id := issueStatusAndAssignee(t, issueID); id != other {
		t.Fatalf("assignee = %s, want the cross-lab override %s", id, other)
	}
}

// A reviewer whose lab is not in the registry is never independent, whether
// the table or an override names it.
func TestReviewerHandler_UnknownLabRefused(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	unknown := reviewerAgent(t, "codex", "model-not-in-the-lab-map")
	c := newReviewerCase(t, author, unknown)

	issueID := c.issue(t, author)
	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "unknown")

	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c = newReviewerCase(t, author, openai)
	issueID = c.issue(t, author)
	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{
		"status": "in_review", "handoff_assignee": overrideTo(unknown),
	}), issueID, "in_progress", "unknown")
}

// An author whose lab is unknown cannot be shown different from the reviewer.
func TestReviewerHandler_UnknownAuthorLabRefused(t *testing.T) {
	unknownAuthor := reviewerAgent(t, "codex", "model-not-in-the-lab-map")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, unknownAuthor, openai)
	issueID := c.issue(t, unknownAuthor)
	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "unknown")
}

// A sticky assignee for the step is checked like an override.
func TestReviewerHandler_StickyAssigneeIsChecked(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	sameLab := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	c := newReviewerCase(t, author, openai)
	issueID := c.issue(t, author)
	dbfx.Exec(t, `INSERT INTO issue_step_assignee (issue_id, status_key, workspace_id, assignee_type, assignee_id, set_by_type, set_by_id)
		VALUES ($1, 'in_review', $2, 'agent', $3, 'member', $4)`, issueID, testWorkspaceID, sameLab, testUserID)

	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "anthropic")
}

// With no capability_routing entry for the issue's type no reviewer can be
// shown independent, so the move is refused and the issue left unchanged.
func TestReviewerHandler_NoRoutingEntryRefuses(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, author, openai)
	setWorkspaceSettings(t, `{}`)
	issueID := c.issue(t, author)
	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "no reviewer")

	// A title without a Conventional Commit type has nothing to route by.
	untyped := dbfx.Issue(t, "reviewer routing without a type "+workflowTestSuffix(), testutil.Cols{"project_id": c.projectID, "status": "in_progress"})
	wantReviewerRefusal(t, updateIssueForTest(t, untyped, map[string]any{"status": "in_review"}), untyped, "in_progress", "Conventional Commit")
}

// A status move that names the assignee itself still enters the reviewer step:
// an author or a same-lab agent is refused, a cross-lab one is accepted.
func TestReviewerHandler_ExplicitAssigneeWithStatusIsChecked(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	sameLab := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, author, openai)
	issueID := c.issue(t, author)
	_, beforeType, beforeID := issueStatusAndAssignee(t, issueID)

	for name, who := range map[string]string{"the author": author, "a same-lab agent": sameLab} {
		wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{
			"status": "in_review", "assignee_type": "agent", "assignee_id": who,
		}), issueID, "in_progress", "anthropic")
		if _, typ, id := issueStatusAndAssignee(t, issueID); typ != beforeType || id != beforeID {
			t.Fatalf("%s: assignee = (%s, %s) after the refusal, want it unchanged (%s, %s)", name, typ, id, beforeType, beforeID)
		}
	}

	updateIssueForTest(t, issueID, map[string]any{
		"status": "in_review", "assignee_type": "agent", "assignee_id": openai,
	}).Want(http.StatusOK)
	if status, _, id := issueStatusAndAssignee(t, issueID); status != "in_review" || id != openai {
		t.Fatalf("issue = (%s, %s), want in_review assigned to the cross-lab reviewer %s", status, id, openai)
	}
}

// The batch endpoint skips an issue it cannot move: the same explicit-assignee
// bypass is closed there, and status and assignee stay as they were.
func TestReviewerHandler_BatchExplicitAssigneeIsChecked(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	sameLab := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, author, openai)
	issueID := c.issue(t, author)
	_, beforeType, beforeID := issueStatusAndAssignee(t, issueID)

	batch := func(assignee string) {
		t.Helper()
		r := newRequest("POST", "/api/issues/batch-update?workspace_id="+testWorkspaceID, map[string]any{
			"issue_ids": []string{issueID},
			"updates":   map[string]any{"status": "in_review", "assignee_type": "agent", "assignee_id": assignee},
		})
		testutil.Call(t, testHandler.BatchUpdateIssues, r)
	}
	batch(sameLab)
	if status, typ, id := issueStatusAndAssignee(t, issueID); status != "in_progress" || typ != beforeType || id != beforeID {
		t.Fatalf("issue = (%s, %s, %s) after a same-lab batch, want it unchanged (in_progress, %s, %s)", status, typ, id, beforeType, beforeID)
	}
	batch(openai)
	if status, _, id := issueStatusAndAssignee(t, issueID); status != "in_review" || id != openai {
		t.Fatalf("issue = (%s, %s) after a cross-lab batch, want in_review assigned to %s", status, id, openai)
	}
}

// CHE-8929: labs come from the model price table, else the runtime provider
// (service.LabOf), not from MULTICA_VOTE_GATE_LABS. With the registry unset, an
// Anthropic author still gets the OpenAI reviewer, a same-lab reviewer is still
// refused, and an agent that cannot be placed stays unknown and is refused.
func TestReviewerHandler_LabsNeedNoVoteGateRegistry(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	sameLab := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	terra := reviewerAgent(t, "codex", "gpt-6.1-terra")
	unplaceable := reviewerAgent(t, "hermes", "model-not-in-the-price-table")

	t.Run("cross-lab reviewer is picked", func(t *testing.T) {
		c := newReviewerCase(t, author, sameLab, terra)
		t.Setenv("MULTICA_VOTE_GATE_LABS", "")
		issueID := c.issue(t, author)
		updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)
		if status, _, id := issueStatusAndAssignee(t, issueID); status != "in_review" || id != terra {
			t.Fatalf("issue = (%s, %s), want in_review assigned to the OpenAI reviewer %s", status, id, terra)
		}
	})
	t.Run("same-lab reviewer is refused", func(t *testing.T) {
		c := newReviewerCase(t, author, sameLab)
		t.Setenv("MULTICA_VOTE_GATE_LABS", "")
		issueID := c.issue(t, author)
		wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "anthropic")
	})
	t.Run("unplaceable reviewer is refused as unknown", func(t *testing.T) {
		c := newReviewerCase(t, author, unplaceable)
		t.Setenv("MULTICA_VOTE_GATE_LABS", "")
		issueID := c.issue(t, author)
		wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "unknown")
	})
}
