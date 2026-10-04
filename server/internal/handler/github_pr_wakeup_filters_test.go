package handler

import (
	"context"
	"testing"
)

// The merged-PR webhook carries the base and head branches into the rule's
// filters: a base-branch filter lets only the matching merge wake the assignee.
func TestPRMergeWebhookAppliesScopedBranchFilters(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1140001
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1140 branch filter", nil)
	// The rule lives on the issue the PR links to, here the child.
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	prRuns := func() int {
		var n int
		if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'='pr_merged'`, fx.child.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config) VALUES($1,'workspace',$1,'pr_merged',false,'{"v":1,"filters":{"base_branch":"release"}}')`,
		testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup_definition WHERE workspace_id=$1`, testWorkspaceID)
	})

	merge := func(number int, base string) {
		body := buildMergedPRWebhookBody(fx.child.Identifier, number, "acme", "widget", installationID)
		body["pull_request"].(map[string]any)["base"] = map[string]any{"ref": base}
		postSignedGitHubWebhook(t, fx.secret, body, "")
	}
	merge(114001, "main")
	if got := prRuns(); got != 0 {
		t.Fatalf("a merge into another base branch woke the assignee: %d runs", got)
	}
	merge(114002, "release")
	if got := prRuns(); got != 1 {
		t.Fatalf("a merge into the filtered base branch: %d runs, want 1", got)
	}
}
