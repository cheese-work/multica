package handler

import (
	"net/http"
	"strings"
	"testing"
)

// CHE-6970 (CHE-6940 step 3): the same auto_close setting and predicates close a
// clean child issue on entry to merged, and each linked PR must also have
// merged into its repository's default branch at the head the PASS names.

var autoCloseMergedSteps = []map[string]any{
	{"status_key": "in_progress"},
	{"status_key": "agent_review", "edges": []map[string]any{
		{"to": "in_progress", "guard": "agent_verdict(CHANGES)"},
		{"to": "in_review", "guard": "agent_verdict(PASS)"},
	}},
	{"status_key": "in_review", "edges": []map[string]any{{"to": "merged", "guard": "agent_done"}}},
	{"status_key": "merged", "edges": []map[string]any{{"to": "done", "guard": "agent_done"}}},
	{"status_key": "blocked"},
	{"status_key": "done"},
}

// newAutoCloseMerged is newAutoClose for a workflow whose merged step hands to
// the acceptor, with the issue waiting at in_review.
func newAutoCloseMerged(t *testing.T, mode string) autoClose {
	t.Helper()
	c := newAutoClose(t, mode)
	createTestCustomStatus(t, "merged", "started")
	wf := createTestWorkflowSteps(t, "Auto close merged "+workflowTestSuffix(), autoCloseMergedSteps)
	setProjectWorkflow(t, c.projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "merged", handToAgent(c.acceptor, "Close it.")).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_review' WHERE id = $1`, c.issueID)
	c.step = "merged"
	return c
}

func (c autoClose) enterMerged(t *testing.T) {
	t.Helper()
	updateIssueForTest(t, c.issueID, map[string]any{"status": "merged"}).Want(http.StatusOK)
}

// mergedPR links a merged PR at head that was merged into base, in a repository
// whose default branch is defaultBranch ("" is a delivery that did not carry it).
func (c autoClose) mergedPR(t *testing.T, head, base, defaultBranch string) {
	t.Helper()
	prID := c.newPR(t, head)
	dbfx.Exec(t, `UPDATE github_pull_request SET state = 'merged', base_ref = $2, default_branch = $3 WHERE id = $1`, prID, base, defaultBranch)
}

// Red test: a clean child enters merged. Today the handler's run starts.
func TestAutoCloseMerged_CleanChildClosesThroughDoneEdgeWithNoRun(t *testing.T) {
	c := newAutoCloseMerged(t, "block")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	c.enterMerged(t)
	c.wantClosed(t)
	var details string
	dbfx.QueryRow(t, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'auto_closed'`, c.issueID).Scan(&details)
	for _, p := range []string{"done_edge", "has_parent", "verdict_pass_current", "no_open_child", "no_open_decision", "no_live_wakeup", "no_cheese_stop", "parent_not_stopped", "post_close_work_none"} {
		if !strings.Contains(details, p) {
			t.Errorf("auto_closed details %s do not name predicate %q", details, p)
		}
	}
}

func TestAutoCloseMerged_PRIssueClosesOnlyWhenMergedAtThePassHeadIntoTheDefaultBranch(t *testing.T) {
	for name, tc := range map[string]struct {
		passHead, prHead, base, defaultBranch string
		closes                                bool
	}{
		"merged at the PASS head into the default branch": {autoCloseHead, autoCloseHead, "main", "main", true},
		"merged at another head":                          {autoCloseHead, autoCloseHead2, "main", "main", false},
		"stacked PR merged into another branch":           {autoCloseHead, autoCloseHead, "agent/step-1", "main", false},
		"default branch not recorded":                     {autoCloseHead, autoCloseHead, "main", "", false},
		"base branch not recorded":                        {autoCloseHead, autoCloseHead, "", "main", false},
	} {
		t.Run(name, func(t *testing.T) {
			c := newAutoCloseMerged(t, "block")
			c.seedVerdict(t, "review_passed", "[]", tc.passHead, "1 hour")
			c.mergedPR(t, tc.prHead, tc.base, tc.defaultBranch)
			c.enterMerged(t)
			if tc.closes {
				c.wantClosed(t)
			} else {
				c.wantHandlerRun(t)
			}
		})
	}
}

func TestAutoCloseMerged_OneUnmergedLinkedPRKeepsTheHandlerRun(t *testing.T) {
	c := newAutoCloseMerged(t, "block")
	c.seedVerdict(t, "review_passed", "[]", autoCloseHead, "1 hour")
	c.mergedPR(t, autoCloseHead, "main", "main")
	c.newPR(t, autoCloseHead2) // a second linked PR, still open
	c.enterMerged(t)
	c.wantHandlerRun(t)
}

// The step 2 negative cases hold on entry to merged too.
func TestAutoCloseMerged_EachFailedPredicateKeepsTheHandlerRun(t *testing.T) {
	for name, tc := range autoCloseNegatives {
		t.Run(name, func(t *testing.T) {
			c := newAutoCloseMerged(t, "block")
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			tc.setup(t, c)
			c.enterMerged(t)
			c.wantHandlerRun(t)
		})
	}
}

func TestAutoCloseMerged_WarnKeepsTheRunAndLogsTheWouldClose(t *testing.T) {
	c := newAutoCloseMerged(t, "warn")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	c.enterMerged(t)
	c.wantHandlerRun(t)
	if n := c.activities(t, "auto_close_would_close"); n != 1 {
		t.Fatalf("auto_close_would_close rows = %d, want 1", n)
	}
	var details string
	dbfx.QueryRow(t, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'auto_close_would_close'`, c.issueID).Scan(&details)
	if !strings.Contains(details, "run_id") {
		t.Fatalf("details %s do not name the run that followed", details)
	}
}

func TestAutoCloseMerged_OffRunsAsToday(t *testing.T) {
	c := newAutoCloseMerged(t, "off")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	c.enterMerged(t)
	c.wantHandlerRun(t)
	if n := c.activities(t, "auto_close_would_close"); n != 0 {
		t.Fatalf("would_close rows = %d, want 0", n)
	}
}

func TestAutoCloseMerged_WorkflowWithoutMergedDoneEdgeIsNeverClosed(t *testing.T) {
	c := newAutoCloseMerged(t, "block")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	wf := createTestWorkflowSteps(t, "No merged done edge "+workflowTestSuffix(), []map[string]any{
		{"status_key": "in_progress"}, {"status_key": "agent_review"},
		{"status_key": "in_review", "edges": []map[string]any{{"to": "merged", "guard": "agent_done"}}},
		{"status_key": "merged"}, {"status_key": "blocked"}, {"status_key": "done"},
	})
	setProjectWorkflow(t, c.projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, c.projectID, "merged", handToAgent(c.acceptor, "Close it.")).Want(http.StatusOK)
	c.enterMerged(t)
	c.wantHandlerRun(t)
}

// The merged webhook delivery records the branch the PR targeted and the
// repository's default branch; a later delivery that omits them keeps them.
func TestAutoCloseMerged_WebhookRecordsBaseAndDefaultBranch(t *testing.T) {
	const secret, installationID = "base-default-secret", int64(30264777)
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issue := prAutoCompleteTestIssue(t, "base and default branch", installationID)
	firePRWebhook(t, secret, installationID, 7, issue.Identifier+": stacked", "", "feat/base", "opened")
	firePRWebhook(t, secret, installationID, 7, issue.Identifier+": stacked", "", "feat/base", "merged")
	read := func() (base, def string) {
		dbfx.QueryRow(t, `SELECT pr.base_ref, pr.default_branch FROM github_pull_request pr
			JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id WHERE ipr.issue_id = $1`, issue.ID).Scan(&base, &def)
		return base, def
	}
	if base, def := read(); base != "main" || def != "main" {
		t.Fatalf("base_ref = %q, default_branch = %q, want main and main", base, def)
	}
	dbfx.Exec(t, `UPDATE github_pull_request SET base_ref = 'agent/step-1' WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id = $1)`, issue.ID)
	firePRWebhook(t, secret, installationID, 7, issue.Identifier+": stacked", "", "feat/base", "edited_merged")
	if base, def := read(); base != "main" || def != "main" {
		t.Fatalf("a later delivery: base_ref = %q, default_branch = %q, want main and main", base, def)
	}
}
