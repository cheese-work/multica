package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9318: an untyped or unlisted title falls back to the "default" routing
// entry for the author and reviewer handlers, an in-flight issue keeps the
// agent that already worked it, and a same_as step on agent_review hands the
// issue to the agent that signed the PASS for the exact head.

func routeDefault(t *testing.T, author string, reviewers ...string) {
	t.Helper()
	setWorkspaceSettings(t, mustJSON(t, map[string]any{
		"capability_routing": map[string]any{
			"feat":    map[string]any{"author": seededReadyAgentID(t), "reviewers": reviewers},
			"default": map[string]any{"author": author, "reviewers": reviewers},
		},
	}))
}

func TestAuthorHandler_UntypedAndUnlistedTitlesTakeTheDefaultEntry(t *testing.T) {
	for _, title := range []string{"plain title with no prefix", "task(workflow): unlisted type", "ops: unlisted type"} {
		t.Run(title, func(t *testing.T) {
			c := newAuthorCase(t)
			routeDefault(t, c.otherAuthor, c.reviewer)
			issueID := dbfx.Issue(t, title, testutil.Cols{"project_id": c.projectID, "status": "todo"})

			updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

			if _, typ, id := issueStatusAndAssignee(t, issueID); typ != "agent" || id != c.otherAuthor {
				t.Fatalf("assignee = (%s, %s), want the default author %s", typ, id, c.otherAuthor)
			}
		})
	}
}

func TestAuthorHandler_TypedTitleStillTakesItsOwnEntryOverTheDefault(t *testing.T) {
	c := newAuthorCase(t)
	routeDefault(t, c.otherAuthor, c.reviewer)
	issueID := c.issue(t) // feat(x): ...

	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != seededReadyAgentID(t) {
		t.Fatalf("assignee = %s, want the feat author %s, not the default", id, seededReadyAgentID(t))
	}
}

func TestAuthorHandler_InFlightIssueKeepsItsExecutorWithoutAStickyRow(t *testing.T) {
	c := newAuthorCase(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, c.authorID).Scan(&runtimeID)
	for _, title := range []string{"plain title with no prefix", "task(workflow): unlisted type", "feat(x): routed type"} {
		t.Run(title, func(t *testing.T) {
			setWorkspaceSettings(t, `{}`) // no table at all
			issueID := dbfx.Issue(t, title, testutil.Cols{"project_id": c.projectID, "status": "todo"})
			dbfx.Task(t, c.otherAuthor, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed"})

			updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

			if _, typ, id := issueStatusAndAssignee(t, issueID); typ != "agent" || id != c.otherAuthor {
				t.Fatalf("assignee = (%s, %s), want the executor %s", typ, id, c.otherAuthor)
			}
		})
	}
}

func TestReviewerHandler_UntypedTitleTakesTheDefaultEntry(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, author, openai)
	routeDefault(t, author, openai)
	issueID := dbfx.Issue(t, "task(workflow): unlisted type", testutil.Cols{"project_id": c.projectID, "status": "in_progress"})
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, author).Scan(&runtimeID)
	dbfx.Task(t, author, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "applied_model": "claude-sonnet-5-5"})

	updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != openai {
		t.Fatalf("assignee = %s, want the default entry's OpenAI reviewer %s", id, openai)
	}
}

func TestReviewerHandler_NoDefaultEntryRefusesAndNamesTheBlocker(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, author, openai) // feat only
	issueID := dbfx.Issue(t, "task(workflow): unlisted type", testutil.Cols{"project_id": c.projectID, "status": "in_progress"})

	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "blocker")
}

func TestReviewerHandler_DefaultEntryWithOnlySameLabReviewersRefuses(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	sameLab := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	c := newReviewerCase(t, author, sameLab)
	routeDefault(t, author, sameLab)
	issueID := dbfx.Issue(t, "plain title with no prefix", testutil.Cols{"project_id": c.projectID, "status": "in_progress"})
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, author).Scan(&runtimeID)
	dbfx.Task(t, author, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "applied_model": "claude-sonnet-5-5"})

	wantReviewerRefusal(t, updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}), issueID, "in_progress", "blocker", "anthropic")
}

// signerCase is a project where agent_review hands to a reviewer agent and
// in_review and merged follow it with same_as agent_review.
type signerCase struct {
	projectID      string
	signer, other  string
	configuredBlkd string
}

func newSignerCase(t *testing.T) signerCase {
	t.Helper()
	createTestCustomStatus(t, "agent_review", "started")
	createTestCustomStatus(t, "merged", "started")
	c := signerCase{
		signer:         reviewerAgent(t, "codex", "gpt-5.5"),
		other:          reviewerAgent(t, "codex", "gpt-5.5"),
		configuredBlkd: reviewerAgent(t, "codex", "gpt-5.5"),
	}
	c.projectID = createWorkflowTestProject(t, "Signer project "+workflowTestSuffix())
	sameAsReview := map[string]any{"handler": map[string]any{"type": "same_as", "step": "agent_review"}, "instructions": "Merge it."}
	setColumnHandoff(t, c.projectID, "agent_review", handToAgent(c.other, "Review it.")).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "in_review", sameAsReview).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "merged", sameAsReview).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "blocked", handToAgent(c.configuredBlkd, "Recover it.")).Want(http.StatusOK)
	return c
}

// issue is in agent_review, assigned to the history agent T (other), with a PR
// at head.
func (c signerCase) issue(t *testing.T, head string) string {
	t.Helper()
	issueID := dbfx.Issue(t, "feat(x): signer", testutil.Cols{"project_id": c.projectID, "status": "agent_review"})
	updateIssueForTest(t, issueID, map[string]any{"assignee_type": "agent", "assignee_id": c.other}).Want(http.StatusOK)
	linkPR(t, issueID, int32(9318000+dbfx.Count(t, `SELECT count(*) FROM github_pull_request`)), head)
	return issueID
}

func (c signerCase) sign(t *testing.T, issueID, agentID, head, findings string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		SELECT workspace_id, id, 'agent', $2, 'review_passed', jsonb_build_object('findings', $4::text, 'pull_request_id', '', 'head_sha', $3::text)
		FROM issue WHERE id = $1`, issueID, agentID, head, findings)
}

func TestSameAs_AgentReviewHandsToThePassSignerNotTheHistoryAgent(t *testing.T) {
	c := newSignerCase(t)
	issueID := c.issue(t, "sha-pass-1")
	c.sign(t, issueID, c.signer, "sha-pass-1", "[]")

	updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)

	if status, typ, id := issueStatusAndAssignee(t, issueID); status != "in_review" || typ != "agent" || id != c.signer {
		t.Fatalf("issue = (%s, %s, %s), want in_review assigned to the PASS signer %s, not history agent %s", status, typ, id, c.signer, c.other)
	}
}

func TestSameAs_AgentReviewWithNoOrMismatchedPassGoesToBlocked(t *testing.T) {
	cases := map[string]func(t *testing.T, c signerCase, issueID string){
		"no verdict": func(*testing.T, signerCase, string) {},
		"older head": func(t *testing.T, c signerCase, id string) { c.sign(t, id, c.signer, "sha-old", "[]") },
		"empty head": func(t *testing.T, c signerCase, id string) { c.sign(t, id, c.signer, "", "[]") },
		"P1 finding": func(t *testing.T, c signerCase, id string) {
			c.sign(t, id, c.signer, "sha-pass-1", `[{"severity":"P1","summary":"x"}]`)
		},
		"changes verdict": func(t *testing.T, c signerCase, id string) {
			dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details) SELECT workspace_id, id, 'agent', $2, 'review_changes_requested', '{"findings":"[]","head_sha":"sha-pass-1"}'::jsonb FROM issue WHERE id = $1`, id, c.signer)
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			c := newSignerCase(t)
			issueID := c.issue(t, "sha-pass-1")
			seed(t, c, issueID)

			updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)

			status, typ, id := issueStatusAndAssignee(t, issueID)
			if status != "blocked" || typ != "agent" || id != c.configuredBlkd {
				t.Fatalf("issue = (%s, %s, %s), want blocked handed to the blocked handler %s, never an arbitrary reviewer", status, typ, id, c.configuredBlkd)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_signer_missing'`, issueID); n != 1 {
				t.Fatalf("review_signer_missing rows = %d, want 1", n)
			}
		})
	}
}

func TestSameAs_MergedAlsoTakesThePassSigner(t *testing.T) {
	c := newSignerCase(t)
	issueID := c.issue(t, "sha-pass-1")
	c.sign(t, issueID, c.signer, "sha-pass-1", "[]")
	updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)

	updateIssueForTest(t, issueID, map[string]any{"status": "merged"}).Want(http.StatusOK)

	if status, _, id := issueStatusAndAssignee(t, issueID); status != "merged" || id != c.signer {
		t.Fatalf("issue = (%s, %s), want merged assigned to the PASS signer %s", status, id, c.signer)
	}
}

func TestSameAs_NamedReviewerStepKeepsTheRoutedReviewer(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	openai := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, author, openai) // in_review: reviewer handler
	createTestCustomStatus(t, "approved", "started")
	setColumnHandoff(t, c.projectID, "approved", map[string]any{
		"handler": map[string]any{"type": "same_as", "step": "in_review"}, "instructions": "Accept it.",
	}).Want(http.StatusOK)
	issueID := c.issue(t, author)
	dbfx.Exec(t, `UPDATE issue SET title = 'feat(x): reviewer routing' WHERE id = $1`, issueID)

	updateIssueForTest(t, issueID, map[string]any{"status": "approved"}).Want(http.StatusOK)

	if status, typ, id := issueStatusAndAssignee(t, issueID); status != "approved" || typ != "agent" || id != openai {
		t.Fatalf("issue = (%s, %s, %s), want approved assigned to the routed reviewer %s, not nobody", status, typ, id, openai)
	}
}
