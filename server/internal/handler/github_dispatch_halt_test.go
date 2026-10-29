package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// mergedReceipts counts the pr.merged receipts a wakeup for the issue recorded.
func mergedReceipts(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule='pr_merged' AND r.event_type='pr.merged'`, issueID)
}

func TestGitHubDispatchHaltStopsPRWakeupsAndResumes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newPRMergeWebhookFixture(t, 222010)
	agent := createHandlerTestAgent(t, "CHE-2220 halt agent", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agent)
	ws := parseUUID(testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	wakeup := service.IssueWakeupService{Tasks: testHandler.TaskService}
	merged := func(owner, repo string, number int32) {
		t.Helper()
		if err := wakeup.TriggerPullRequestWakeup(ctx, parseUUID(fx.child.ID), service.PullRequestWakeupInput{
			Rule: service.SystemRulePRMerged, RepoOwner: owner, RepoName: repo, Number: number,
			URL: "https://github.com/" + owner + "/" + repo + "/pull/1", MergeCommit: "halt-test",
		}); err != nil {
			t.Fatalf("TriggerPullRequestWakeup: %v", err)
		}
	}
	halt := func(scope string) {
		t.Helper()
		if err := testHandler.Queries.HaltGitHubDispatch(ctx, db.HaltGitHubDispatchParams{WorkspaceID: ws, Scope: scope}); err != nil {
			t.Fatalf("HaltGitHubDispatch(%s): %v", scope, err)
		}
	}
	resume := func(scope string) {
		t.Helper()
		if _, err := testHandler.Queries.ResumeGitHubDispatch(ctx, db.ResumeGitHubDispatchParams{WorkspaceID: ws, Scope: scope}); err != nil {
			t.Fatalf("ResumeGitHubDispatch(%s): %v", scope, err)
		}
	}

	// Switch on for one repo: that repo is dark, a sibling repo still dispatches.
	halt("acme/widget")
	merged("Acme", "Widget", 1)
	if got := mergedReceipts(t, fx.child.ID); got != 0 {
		t.Fatalf("halted repo dispatched: receipts = %d, want 0", got)
	}
	merged("acme", "other", 2)
	if got := mergedReceipts(t, fx.child.ID); got != 1 {
		t.Fatalf("sibling repo receipts = %d, want 1", got)
	}

	// Org scope covers every repo of the owner.
	resume("acme/widget")
	halt("acme")
	merged("acme", "other", 3)
	if got := mergedReceipts(t, fx.child.ID); got != 1 {
		t.Fatalf("halted org dispatched: receipts = %d, want 1", got)
	}

	// Workspace-wide scope covers any owner.
	halt("*")
	merged("elsewhere", "repo", 4)
	if got := mergedReceipts(t, fx.child.ID); got != 1 {
		t.Fatalf("halted workspace dispatched: receipts = %d, want 1", got)
	}

	// Switch off: dispatch resumes.
	resume("acme")
	resume("*")
	merged("acme", "widget", 5)
	if got := mergedReceipts(t, fx.child.ID); got != 2 {
		t.Fatalf("after resume receipts = %d, want 2", got)
	}
}

func setHalt(t *testing.T, scope string, halted bool, machine bool) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("PUT", "/api/github/dispatch-halt", map[string]any{"scope": scope, "halted": halted})
	if machine {
		req.Header.Set("X-Actor-Source", "task_token")
	}
	w := httptest.NewRecorder()
	testHandler.SetGitHubDispatchHalt(w, req)
	return w
}

func readHalts(t *testing.T) []string {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.GetGitHubDispatchHalt(w, newRequest("GET", "/api/github/dispatch-halt", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Halts []struct{ Scope string } `json:"halts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	scopes := []string{}
	for _, h := range out.Halts {
		scopes = append(scopes, h.Scope)
	}
	return scopes
}

func TestGitHubDispatchHaltEndpoint(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	if got := readHalts(t); len(got) != 0 {
		t.Fatalf("fresh workspace halts = %v, want none", got)
	}

	for _, bad := range []string{"", "a/b/c", "owner/", "/repo", "o wner", "*/x"} {
		if w := setHalt(t, bad, true, false); w.Code != http.StatusBadRequest {
			t.Errorf("scope %q: status = %d, want 400", bad, w.Code)
		}
	}

	if w := setHalt(t, " ACME/Widget ", true, false); w.Code != http.StatusOK {
		t.Fatalf("halt: %d %s", w.Code, w.Body)
	}
	if got := readHalts(t); len(got) != 1 || got[0] != "acme/widget" {
		t.Fatalf("halts = %v, want [acme/widget] (lower-cased)", got)
	}
	if w := setHalt(t, "acme/widget", true, false); w.Code != http.StatusOK || len(readHalts(t)) != 1 {
		t.Fatalf("a second halt must be a no-op: %d %v", w.Code, readHalts(t))
	}

	// An agent can switch the halt on but never off.
	if w := setHalt(t, "acme/widget", false, true); w.Code != http.StatusForbidden {
		t.Fatalf("machine resume status = %d, want 403", w.Code)
	}
	if got := readHalts(t); len(got) != 1 {
		t.Fatalf("machine resume lifted the halt: %v", got)
	}
	if w := setHalt(t, "*", true, true); w.Code != http.StatusOK {
		t.Fatalf("machine halt status = %d, want 200", w.Code)
	}

	if w := setHalt(t, "acme/widget", false, false); w.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	if got := readHalts(t); len(got) != 1 || got[0] != "*" {
		t.Fatalf("halts after resume = %v, want [*]", got)
	}
}

func TestGitHubDispatchHaltCancelsLiveRunsAndRevokesTokens(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newPRMergeWebhookFixture(t, 222011)
	linkedPRForWakeup(t, fx, 222011, 7, "halt-sha")
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	agent := createHandlerTestAgent(t, "CHE-2220 sweep agent", nil)
	// A run GitHub started on the PR's issue, and a run on an issue with no PR.
	githubRun := dbfx.Task(t, agent, testutil.Cols{
		"status": "running", "runtime_id": handlerTestRuntimeID(t), "issue_id": fx.child.ID,
		"context": `{"github_review_key":"k"}`,
	})
	otherIssue := dbfx.Issue(t, "CHE-2220 unlinked")
	otherRun := dbfx.Task(t, agent, testutil.Cols{
		"status": "running", "runtime_id": handlerTestRuntimeID(t), "issue_id": otherIssue,
		"context": `{"github_review_key":"k"}`,
	})
	for _, id := range []string{githubRun, otherRun} {
		dbfx.Insert(t, "task_token", testutil.Cols{
			"token_hash": auth.HashToken("mat_halt_" + id), "task_id": id, "agent_id": agent,
			"workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour),
		})
	}

	// A different repo's halt leaves the run alone.
	setHalt(t, "acme/other", true, false)
	if status := taskStatus(t, githubRun); status != "running" {
		t.Fatalf("run status after an unrelated halt = %s, want running", status)
	}

	var out struct {
		RunsRevoked int `json:"runs_revoked"`
	}
	w := setHalt(t, "acme", true, false)
	if w.Code != http.StatusOK {
		t.Fatalf("halt: %d %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.RunsRevoked != 1 {
		t.Fatalf("runs_revoked = %d, want 1", out.RunsRevoked)
	}
	if status := taskStatus(t, githubRun); status != "cancelled" {
		t.Fatalf("GitHub run status = %s, want cancelled", status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_token WHERE task_id=$1`, githubRun); n != 0 {
		t.Fatalf("GitHub run token survived: %d", n)
	}
	if status := taskStatus(t, otherRun); status != "running" {
		t.Fatalf("unlinked run status = %s, want running", status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_token WHERE task_id=$1`, otherRun); n != 1 {
		t.Fatalf("unlinked run token = %d, want 1", n)
	}
}

// The snapshot pipeline wakes the owner of a conflicting PR. A halt silences it
// and, with nothing recorded, the next refresh after the resume wakes once.
func TestGitHubDispatchHaltSilencesSnapshotWakeups(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 222012
	const prNumber int32 = 22201
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-2220 snapshot", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che2220-head")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	refresh := startSnapshotRefresh(t, installationID, prNumber, func() ghsnapshot.PRSnapshot {
		return ghsnapshot.PRSnapshot{HeadSHA: "che2220-head", BaseRef: "main", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY", RollupState: "SUCCESS", HasChecks: true}
	})
	wakeups := func() int { return prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention") }

	setHalt(t, "acme/widget", true, false)
	refresh()
	if got := wakeups(); got != 0 {
		t.Fatalf("halted snapshot queued %d wakeups, want 0", got)
	}
	setHalt(t, "acme/widget", false, false)
	refresh()
	if got := wakeups(); got != 1 {
		t.Fatalf("snapshot after resume queued %d wakeups, want 1", got)
	}
}

// A green head moves a flagged workflow step and starts the next handler. That
// move bypasses the PR wakeup service, so the halt must stop it too.
func TestGitHubDispatchHaltStopsCIGreenAdvance(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	const installationID int64 = 222013
	const prNumber int32 = 22202
	fx := newPRMergeWebhookFixture(t, installationID)
	reviewer := seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "CHE-2220 advance "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "advance_on_ci_green": true},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": reviewer}, "instructions": "Review the PR."},
		{"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "CHE-2220 advance project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET project_id=$2, status='in_progress', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, fx.child.ID, projectID); err != nil {
		t.Fatal(err)
	}
	linkedPRForWakeup(t, fx, installationID, prNumber, "che2220-adv")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	refresh := startSnapshotRefresh(t, installationID, prNumber, newGreenSnapshot("che2220-adv").get)
	status := func() string { s, _, _ := issueStatusAndAssignee(t, fx.child.ID); return s }

	setHalt(t, "acme/widget", true, false)
	refresh()
	if got := status(); got != "in_progress" {
		t.Fatalf("halted green head moved the issue to %s", got)
	}
	if n := countPendingTasksForAgent(t, fx.child.ID, reviewer); n != 0 {
		t.Fatalf("halted green head queued %d handler runs, want 0", n)
	}
	setHalt(t, "acme/widget", false, false)
	refresh()
	if got := status(); got != "in_review" {
		t.Fatalf("green head after the resume left the issue at %s, want in_review", got)
	}
}

// A PR review wakes the PR author through its own path, not the wakeup service.
func TestGitHubDispatchHaltDropsPRReviewWakeup(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	authorRuns := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, fixture.issueID, fixture.author)
	}

	// The webhook route now defers reviews to durable intake; the legacy helper is
	// the only path that still wakes the author, so the halt check is driven there.
	body, err := json.Marshal(fixture.payload)
	if err != nil {
		t.Fatal(err)
	}
	setHalt(t, "acme", true, false)
	if err := fixture.handler.handlePullRequestReviewEvent(context.Background(), body); err != nil {
		t.Fatalf("halted review: %v", err)
	}
	if got := authorRuns(); got != 0 {
		t.Fatalf("halted review woke the author %d times, want 0", got)
	}

	setHalt(t, "acme", false, false)
	if err := fixture.handler.handlePullRequestReviewEvent(context.Background(), body); err != nil {
		t.Fatalf("resumed review: %v", err)
	}
	if got := authorRuns(); got != 1 {
		t.Fatalf("review after the resume woke the author %d times, want 1", got)
	}
}

// A member's pull_request condition rule is evaluated by the wakeup scheduler,
// not by the built-in PR wakeups, so it needs its own halt check. The event is
// dropped: a later resume does not replay it.
func TestGitHubDispatchHaltDropsPRConditionWakeup(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 222091
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := handlerTestAgentID(t)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	pull := linkedPRForWakeup(t, fx, installationID, 222091, "che2220-condition-head")
	ruleID := createRule(t, fx.child.ID, agentID, pgtype.UUID{}, service.WakeupInput{
		Instruction: "Review merged PR", Kind: "event",
		Condition: json.RawMessage(`{"type":"pull_request","event":"merged"}`),
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})

	setHalt(t, "acme/widget", true, false)
	dbfx.Exec(t, `UPDATE github_pull_request SET state='merged' WHERE id=$1`, pull.ID)
	runWakeupTick(t)
	if got := wakeupRunsOf(t, ruleID); got != 0 {
		t.Fatalf("halted PR condition dispatched %d runs, want 0", got)
	}

	setHalt(t, "acme/widget", false, false)
	runWakeupTick(t)
	if got := wakeupRunsOf(t, ruleID); got != 0 {
		t.Fatalf("resume replayed the dropped PR condition: %d runs, want 0", got)
	}
}

// A halted-only change must not ride on an unhalted PR that already matched:
// only a newly satisfied fact outside the halted repo starts a run.
func TestGitHubDispatchHaltIgnoresHaltedChangeBesideLivePR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 222093
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := handlerTestAgentID(t)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	halted := linkedPRForWakeup(t, fx, installationID, 222093, "che2220-beside-head")
	now := time.Now().UTC()
	live, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: installationID,
		RepoOwner: "acme", RepoName: "other", PrNumber: 222094,
		Title: fx.child.Identifier + ": live", State: "merged",
		HtmlUrl:     "https://github.com/acme/other/pull/222094",
		PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		HeadSha:     "che2220-live-head", ClearMergeableState: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}
	if _, err := testHandler.Queries.LinkIssueToPullRequest(context.Background(), db.LinkIssueToPullRequestParams{
		IssueID: parseUUID(fx.child.ID), PullRequestID: live.ID,
	}); err != nil {
		t.Fatalf("LinkIssueToPullRequest: %v", err)
	}
	ruleID := createRule(t, fx.child.ID, agentID, pgtype.UUID{}, service.WakeupInput{
		Instruction: "Review merged PR", Kind: "event", Mode: "continuous",
		Condition: json.RawMessage(`{"type":"pull_request","event":"merged"}`),
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	runWakeupTick(t)
	if got := wakeupRunsOf(t, ruleID); got != 1 {
		t.Fatalf("live merged PR dispatched %d runs, want 1", got)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE context->>'wakeup_id' = $1`, ruleID)

	setHalt(t, "acme/widget", true, false)
	dbfx.Exec(t, `UPDATE github_pull_request SET state='merged' WHERE id=$1`, halted.ID)
	dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() WHERE id = $1`, ruleID)
	runWakeupTick(t)
	if got := wakeupRunsOf(t, ruleID); got != 1 {
		t.Fatalf("halted merge beside a live PR dispatched %d runs in total, want 1", got)
	}
	setHalt(t, "acme/widget", false, false)
	dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() WHERE id = $1`, ruleID)
	runWakeupTick(t)
	if got := wakeupRunsOf(t, ruleID); got != 1 {
		t.Fatalf("resume replayed the halted merge: %d runs in total, want 1", got)
	}
}

// A halted PR that stops matching (its checks go pending on a new head) is a
// halted-only change: a live PR that did not change must not start a run.
func TestGitHubDispatchHaltIgnoresHaltedCheckRemovalBesideLivePR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 222096
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := handlerTestAgentID(t)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	halted := linkedPRForWakeup(t, fx, installationID, 222096, "che2220-removal-halted")
	now := time.Now().UTC()
	live, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: installationID,
		RepoOwner: "acme", RepoName: "other", PrNumber: 222097,
		Title: fx.child.Identifier + ": live", State: "open",
		HtmlUrl:     "https://github.com/acme/other/pull/222097",
		PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		HeadSha:     "che2220-removal-live", ClearMergeableState: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}
	if _, err := testHandler.Queries.LinkIssueToPullRequest(context.Background(), db.LinkIssueToPullRequestParams{
		IssueID: parseUUID(fx.child.ID), PullRequestID: live.ID,
	}); err != nil {
		t.Fatalf("LinkIssueToPullRequest: %v", err)
	}
	testHandler.TaskService.PRSnapshotsEnabled = true
	t.Cleanup(func() {
		testHandler.TaskService.PRSnapshotsEnabled = false
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	finish := func(id pgtype.UUID, head, rollup string) {
		dbfx.Exec(t, `UPDATE github_pull_request SET checks_rollup_state=$3, snapshot_head_sha=$2 WHERE id=$1`, id, head, rollup)
	}
	poll := func(ruleID string) {
		dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() WHERE id = $1`, ruleID)
		runWakeupTick(t)
	}
	finish(halted.ID, "che2220-removal-halted", "SUCCESS")
	finish(live.ID, "che2220-removal-live", "SUCCESS")
	ruleID := createRule(t, fx.child.ID, agentID, pgtype.UUID{}, service.WakeupInput{
		Instruction: "Review finished checks", Kind: "event", Mode: "continuous",
		Condition: json.RawMessage(`{"type":"pull_request","event":"checks_finished"}`),
	})
	finish(live.ID, "che2220-removal-live-2", "SUCCESS")
	poll(ruleID)
	if got := wakeupRunsOf(t, ruleID); got != 1 {
		t.Fatalf("live finished checks dispatched %d runs, want 1", got)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE context->>'wakeup_id' = $1`, ruleID)

	setHalt(t, "acme/widget", true, false)
	finish(halted.ID, "che2220-removal-halted-2", "PENDING")
	poll(ruleID)
	if got := wakeupRunsOf(t, ruleID); got != 1 {
		t.Fatalf("halted check removal beside a live PR dispatched %d runs in total, want 1", got)
	}
	finish(live.ID, "che2220-removal-live-3", "SUCCESS")
	poll(ruleID)
	if got := wakeupRunsOf(t, ruleID); got != 2 {
		t.Fatalf("new live fact during halt dispatched %d runs in total, want 2", got)
	}
	setHalt(t, "acme/widget", false, false)
	poll(ruleID)
	if got := wakeupRunsOf(t, ruleID); got != 2 {
		t.Fatalf("resume replayed the halted change: %d runs in total, want 2", got)
	}
}

// The CI-green mark goes on the run that move enqueued, not on whichever run
// of the issue is newest when the mark is written.
func TestGitHubDispatchHaltMarksOnlyTheCIGreenRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	const installationID int64 = 222095
	fx := newPRMergeWebhookFixture(t, installationID)
	reviewer := seededReadyAgentID(t)
	other := createHandlerTestAgent(t, "CHE-2220 unrelated run", []byte("[]"))
	wf := createTestWorkflowSteps(t, "CHE-2220 mark "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "advance_on_ci_green": true},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": reviewer}, "instructions": "Review the PR."},
		{"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "CHE-2220 mark project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue SET project_id=$2, status='in_progress', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, fx.child.ID, projectID)
	linkedPRForWakeup(t, fx, installationID, 222095, "che2220-mark")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	// A run of another path whose created_at is newer than anything the green
	// snapshot enqueues: the newest-run guess would tag it.
	var otherRuntime string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, other).Scan(&otherRuntime)
	unrelated := dbfx.Task(t, other, testutil.Cols{"issue_id": fx.child.ID, "runtime_id": otherRuntime, "created_at": time.Now().Add(time.Hour)})
	startSnapshotRefresh(t, installationID, 222095, newGreenSnapshot("che2220-mark").get)()
	if got := countPendingTasksForAgent(t, fx.child.ID, reviewer); got != 1 {
		t.Fatalf("CI green queued %d handler runs, want 1", got)
	}

	setHalt(t, "acme/widget", true, false)
	if got := countPendingTasksForAgent(t, fx.child.ID, reviewer); got != 0 {
		t.Fatalf("halt left %d CI-green runs live, want 0", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, unrelated); got != 1 {
		t.Fatal("halt cancelled an unrelated run of the issue")
	}
}

// A CI-green workflow handoff queued before the switch went on is a
// GitHub-triggered run too: switching on cancels it.
func TestGitHubDispatchHaltCancelsCIGreenRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	const installationID int64 = 222092
	fx := newPRMergeWebhookFixture(t, installationID)
	reviewer := seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "CHE-2220 sweep "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "advance_on_ci_green": true},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": reviewer}, "instructions": "Review the PR."},
		{"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "CHE-2220 sweep project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue SET project_id=$2, status='in_progress', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, fx.child.ID, projectID)
	linkedPRForWakeup(t, fx, installationID, 222092, "che2220-sweep")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	startSnapshotRefresh(t, installationID, 222092, newGreenSnapshot("che2220-sweep").get)()
	if got := countPendingTasksForAgent(t, fx.child.ID, reviewer); got != 1 {
		t.Fatalf("CI green queued %d handler runs, want 1", got)
	}

	setHalt(t, "acme/widget", true, false)
	if got := countPendingTasksForAgent(t, fx.child.ID, reviewer); got != 0 {
		t.Fatalf("halt left %d CI-green runs live, want 0", got)
	}
}
