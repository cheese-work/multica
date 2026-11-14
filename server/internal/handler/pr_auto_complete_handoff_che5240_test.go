package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5240: a PR merge that moves an issue into a workflow step with a handler
// hands the issue to that handler and starts its run, as the pr.merged edge
// did. A step without a handler, or no workflow step, keeps the upstream
// status-only write.

var prHandoffSteps = []map[string]any{
	{"status_key": "backlog"}, {"status_key": "todo"}, {"status_key": "in_progress"},
	{"status_key": "in_review"}, {"status_key": "merged"}, {"status_key": "blocked"},
}

// prHandoffIssue is an issue with a linked-by-title PR sitting in_review in a
// project on a workflow with prHandoffSteps. handler true hands the merged
// step to an agent. It returns the issue and that agent.
func prHandoffIssue(t *testing.T, installationID int64, handler bool) (issueID, identifier, agentID string) {
	t.Helper()
	seedTestCatalog(t)
	createTestCustomStatus(t, "merged", "started")
	agentID = seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "PR handoff "+workflowTestSuffix(), prHandoffSteps)
	if handler {
		setWorkflowHandoff(t, wf.ID, "merged", handToAgent(agentID, "Verify landing.")).Want(http.StatusOK)
	}
	projectID := createWorkflowTestProject(t, "PR handoff project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issue := prAutoCompleteTestIssue(t, "pr handoff", installationID)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET project_id=$2, status='in_review', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, issue.ID, projectID); err != nil {
		t.Fatal(err)
	}
	return issue.ID, issue.Identifier, agentID
}

func wantHandedOff(t *testing.T, issueID, agentID string) {
	t.Helper()
	status, assigneeType, assigneeID := issueStatusAndAssignee(t, issueID)
	if status != "merged" || assigneeType != "agent" || assigneeID != agentID {
		t.Fatalf("status=%q assignee=%s/%s, want merged handed to agent %s", status, assigneeType, assigneeID, agentID)
	}
	if n := taskCountFor(t, issueID, agentID); n != 1 {
		t.Fatalf("handler runs = %d, want 1", n)
	}
}

func TestPRMergeHandsOffToStepHandler(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5240001
	secret := "che5240-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")

	firePRWebhook(t, secret, installationID, 1, identifier+": work", "", "feat/che5240", "merged")

	wantHandedOff(t, issueID, agentID)
	rows := statusRows(t, issueID)
	if len(rows) != 1 {
		t.Fatalf("status_changed rows = %d, want 1", len(rows))
	}
	trigger, _ := rows[0]["trigger"].(map[string]any)
	cause, _ := trigger["cause"].(map[string]any)
	if cause["kind"] != "pr_merged" {
		t.Fatalf("cause.kind = %v, want pr_merged", cause["kind"])
	}
	if rows[0]["pull_requests"] != "#1" || rows[0]["source"] != "pr_automation" {
		t.Fatalf("timeline details = %v, want pull_requests #1 from pr_automation", rows[0])
	}
	want := []any{map[string]any{"repo_owner": "acme", "repo_name": "widget", "pr_number": float64(1), "merge_commit_sha": "merge1"}}
	got, _ := json.Marshal(cause["prs"])
	wantJSON, _ := json.Marshal(want)
	if string(got) != string(wantJSON) {
		t.Fatalf("cause.prs = %s, want %s", got, wantJSON)
	}
}

// A step with no handler keeps the upstream write: the status moves, nobody is
// assigned and no run starts. The history row still carries the PR refs.
func TestPRMergeIntoStepWithoutHandlerStartsNoRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5240002
	secret := "che5240-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, false)
	setWorkspacePRMergeStatusForTest(t, "merged")

	firePRWebhook(t, secret, installationID, 2, identifier+": work", "", "feat/che5240b", "merged")

	status, assigneeType, _ := issueStatusAndAssignee(t, issueID)
	if status != "merged" || assigneeType != "" {
		t.Fatalf("status=%q assignee type=%q, want merged and unassigned", status, assigneeType)
	}
	if n := taskCountFor(t, issueID, agentID); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
	rows := statusRows(t, issueID)
	trigger, _ := rows[0]["trigger"].(map[string]any)
	cause, _ := trigger["cause"].(map[string]any)
	prs, _ := cause["prs"].([]any)
	if len(rows) != 1 || cause["kind"] != "pr_merged" || len(prs) != 1 {
		t.Fatalf("rows=%d cause=%v, want one pr_merged row with one PR ref", len(rows), cause)
	}
}

// The workflow check follows the issue's own workflow, not its project's.
func TestPRMergeUsesIssueLevelWorkflow(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	secret := "che5240-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	t.Run("issue workflow without the target writes nothing", func(t *testing.T) {
		const installationID int64 = 5240003
		issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
		narrow := createTestWorkflow(t, "No merged step "+workflowTestSuffix(), "in_review")
		if _, err := testPool.Exec(context.Background(), `UPDATE issue SET workflow_id=$2 WHERE id=$1`, issueID, narrow.ID); err != nil {
			t.Fatal(err)
		}
		setWorkspacePRMergeStatusForTest(t, "merged")

		firePRWebhook(t, secret, installationID, 3, identifier+": work", "", "feat/che5240c", "merged")

		if got := issueStatusForTest(t, issueID); got != "in_review" {
			t.Fatalf("status = %q, want in_review: the issue's workflow has no merged step", got)
		}
		if n := taskCountFor(t, issueID, agentID); n != 0 {
			t.Fatalf("runs = %d, want 0", n)
		}
		if n := len(statusRows(t, issueID)); n != 0 {
			t.Fatalf("status_changed rows = %d, want 0", n)
		}
	})

	t.Run("issue workflow handler wins over the project's", func(t *testing.T) {
		const installationID int64 = 5240004
		issueID, identifier, agentID := prHandoffIssue(t, installationID, false)
		own := createTestWorkflowSteps(t, "Own handler "+workflowTestSuffix(), prHandoffSteps)
		setWorkflowHandoff(t, own.ID, "merged", handToAgent(agentID, "Verify landing.")).Want(http.StatusOK)
		if _, err := testPool.Exec(context.Background(), `UPDATE issue SET workflow_id=$2 WHERE id=$1`, issueID, own.ID); err != nil {
			t.Fatal(err)
		}
		setWorkspacePRMergeStatusForTest(t, "merged")

		firePRWebhook(t, secret, installationID, 4, identifier+": work", "", "feat/che5240d", "merged")

		wantHandedOff(t, issueID, agentID)
	})
}

// Two merges deciding at once move the issue once and start one run.
func TestPRMergeHandoffConcurrentMergesMoveOnce(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5240005
	secret := "che5240-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	// Merged PR, linked by title; the webhook's own decision runs once, then
	// two more decisions race.
	firePRWebhook(t, secret, installationID, 5, identifier+": work", "", "feat/che5240e", "opened")
	if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET state='merged' WHERE workspace_id=$1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = testHandler.autoCompleteIssue(context.Background(), parseUUID(testWorkspaceID), parseUUID(issueID), nil)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("autoCompleteIssue: %v", err)
		}
	}

	wantHandedOff(t, issueID, agentID)
	if n := len(statusRows(t, issueID)); n != 1 {
		t.Fatalf("status_changed rows = %d, want 1", n)
	}
}

// A PR linked between the decision and the write keeps the issue where it is,
// on the handoff path too.
func TestPRMergeHandoffPRLinkedDuringCompletionKeepsIssueOpen(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5240006
	secret := "che5240-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	firePRWebhook(t, secret, installationID, 6, identifier+": work", "", "feat/che5240f", "opened")
	original := testHandler.TxStarter
	t.Cleanup(func() { testHandler.TxStarter = original })
	testHandler.TxStarter = hookedTxStarter{base: original, hook: func() error {
		testHandler.TxStarter = original
		firePRWebhook(t, secret, installationID, 7, identifier+": more", "", "feat/che5240g", "opened")
		return nil
	}}

	firePRWebhook(t, secret, installationID, 6, identifier+": work", "", "feat/che5240f", "merged")

	if n := linkedPRCountForTest(t, issueID); n != 2 {
		t.Fatalf("want 2 linked PRs, got %d", n)
	}
	if got := issueStatusForTest(t, issueID); got != "in_review" {
		t.Fatalf("status = %q, want in_review with an open linked PR", got)
	}
	if n := taskCountFor(t, issueID, agentID); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
}

// Unlinking the last open PR, and linking a merged one, re-run the decision
// and hand off.
func TestPRMergeHandoffAfterUnlinkAndLink(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	secret := "che5240-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	t.Run("unlink", func(t *testing.T) {
		const installationID int64 = 5240007
		issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
		setWorkspacePRMergeStatusForTest(t, "merged")
		firePRWebhook(t, secret, installationID, 8, identifier+": a", "", "feat/che5240h", "opened")
		firePRWebhook(t, secret, installationID, 9, identifier+": b", "", "feat/che5240i", "opened")
		firePRWebhook(t, secret, installationID, 8, identifier+": a", "", "feat/che5240h", "merged")
		if got := issueStatusForTest(t, issueID); got != "in_review" {
			t.Fatalf("status = %q, want in_review while PR 9 is open", got)
		}
		for _, pr := range listIssuePRsForTest(t, issueID).PullRequests {
			if pr.Number == 9 {
				unlinkPRForTest(t, issueID, pr.ID)
			}
		}
		wantHandedOff(t, issueID, agentID)
	})

	t.Run("link", func(t *testing.T) {
		const installationID int64 = 5240008
		issueID, _, agentID := prHandoffIssue(t, installationID, true)
		setWorkspacePRMergeStatusForTest(t, "merged")
		firePRWebhook(t, secret, installationID, 10, "Unrelated refactor", "", "refactor/che5240", "merged")
		req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/pull-requests", map[string]any{"url": "https://github.com/acme/widget/pull/10"}), "id", issueID)
		testutil.Call(t, testHandler.LinkIssuePullRequest, req).Want(http.StatusOK)
		wantHandedOff(t, issueID, agentID)
	})
}

// A merge on a self-hosted forge moves the issue and hands off the same way.
func TestPRMergeHandoffFromVCSWebhook(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	box := withVCSBox(t)
	connID := seedVCSConnection(t, ctx, box, "forgejo", "https://forgejo.test")
	issueID, identifier, agentID := prHandoffIssue(t, 5240009, true)
	t.Cleanup(func() { cleanupVCS(ctx, issueID) })
	setWorkspacePRMergeStatusForTest(t, "merged")

	fire := func(action, state string, merged bool) {
		raw, _ := json.Marshal(map[string]any{
			"action": action,
			"pull_request": map[string]any{
				"number": 11, "html_url": "https://forgejo.test/acme/widget/pulls/11",
				"title": identifier + ": work", "body": "", "state": state, "merged": merged,
				"created_at": "2026-05-01T00:00:00Z", "updated_at": "2026-05-02T00:00:00Z",
				"merged_at": "2026-05-02T00:00:00Z",
				"head":      map[string]any{"ref": "feat/che5240", "sha": "def"},
				"user":      map[string]any{"username": "octo"},
			},
			"repository": map[string]any{"name": "widget", "owner": map[string]any{"username": "acme"}},
		})
		w := httptest.NewRecorder()
		testHandler.HandleVCSWebhook(w, vcsWebhookReq(connID, map[string]string{
			"X-Gitea-Event": "pull_request", "X-Gitea-Signature": giteaSig(raw),
		}, raw))
		if w.Code != http.StatusAccepted {
			t.Fatalf("%s event: %d %s", action, w.Code, w.Body.String())
		}
	}
	fire("opened", "open", false)
	fire("closed", "closed", true)

	wantHandedOff(t, issueID, agentID)
}
