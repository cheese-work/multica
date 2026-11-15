package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5241: a PR merge that hands the issue to a step handler settles the
// merge's wake with the handler's run: one run per merge. The wake's receipt is
// reserved to the inserted run (ReserveWakeupReceipts) and counts as handled
// once that run starts; a run that ends unstarted gives it back.

type prMergeReceipt struct {
	Key       string
	TaskID    string
	Processed bool
}

func prMergeReceipts(t *testing.T, issueID string) []prMergeReceipt {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT r.event_key, COALESCE(r.task_id::text, ''), r.processed_at IS NOT NULL
		FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id = r.wakeup_id
		WHERE w.issue_id = $1 AND w.system_rule = 'pr_merged' ORDER BY r.event_key`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []prMergeReceipt
	for rows.Next() {
		var r prMergeReceipt
		if err := rows.Scan(&r.Key, &r.TaskID, &r.Processed); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func prMergeReceiptFor(t *testing.T, issueID string, number int) prMergeReceipt {
	t.Helper()
	key := "github:acme/widget#" + itoa(number)
	for _, r := range prMergeReceipts(t, issueID) {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no %s receipt on issue %s; have %+v", key, issueID, prMergeReceipts(t, issueID))
	return prMergeReceipt{}
}

func issueTaskIDs(t *testing.T, issueID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id::text FROM agent_task_queue WHERE issue_id = $1 ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func prMergeWakeRuns(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND context->>'wakeup_system' = 'pr_merged'`, issueID)
}

// finishRun starts a queued run and completes it, as a daemon would.
func finishRun(t *testing.T, taskID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed', dispatched_at=COALESCE(dispatched_at, now()), started_at=COALESCE(started_at, now()), completed_at=now() WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
}

// dispatchMergeWake runs the issue's pr_merged rule once, as a scheduler pass.
func dispatchMergeWake(t *testing.T, issueID string) {
	t.Helper()
	var id pgtype.UUID
	if err := testPool.QueryRow(context.Background(), `SELECT id FROM issue_wakeup WHERE issue_id = $1 AND system_rule = 'pr_merged'`, issueID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	wakeup := service.IssueWakeupService{Tasks: testHandler.TaskService}
	if err := wakeup.DispatchRule(context.Background(), parseUUID(testWorkspaceID), id); err != nil {
		t.Fatalf("dispatch pr_merged rule: %v", err)
	}
}

// triggerMergeWake delivers the merge wake for PR number, as the webhook does.
func triggerMergeWake(t *testing.T, issueID string, number int) {
	t.Helper()
	wakeup := service.IssueWakeupService{Tasks: testHandler.TaskService}
	err := wakeup.TriggerPullRequestWakeup(context.Background(), parseUUID(issueID), service.PullRequestWakeupInput{
		Rule: service.SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: int32(number),
		URL: "https://github.com/acme/widget/pull/" + itoa(number), MergeCommit: "merge" + itoa(number),
	})
	if err != nil {
		t.Fatalf("TriggerPullRequestWakeup: %v", err)
	}
}

// busyWithCurrentRun gives the issue an agent that is running now, so a wake
// defers behind that run instead of starting its own.
func busyWithCurrentRun(t *testing.T, issueID, agentID string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE issue SET assignee_type='agent', assignee_id=$2 WHERE id=$1`, issueID, agentID); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
	if err != nil {
		t.Fatalf("enqueue current run: %v", err)
	}
	id := uuidToString(task.ID)
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status='running', dispatched_at=now(), started_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func prMergeOpenChild(t *testing.T, parentID string) string {
	t.Helper()
	// An open child blocks the parent by status only when the workflow does not
	// count the children's PRs (include_child_prs, default true).
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_workflow SET include_child_prs = false
		WHERE id = (SELECT p.workflow_id FROM issue i JOIN project p ON p.id = i.project_id WHERE i.id = $1)`, parentID); err != nil {
		t.Fatal(err)
	}
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "open child", "status": "todo", "parent_issue_id": parentID,
	})
	w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
	var child IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, child.ID) })
	return child.ID
}

func prMergeAutoComplete(t *testing.T, issueID string) {
	t.Helper()
	if err := testHandler.autoCompleteIssue(context.Background(), parseUUID(testWorkspaceID), parseUUID(issueID), nil); err != nil {
		t.Fatalf("autoCompleteIssue: %v", err)
	}
}

// Normal merge: the decision hands off before the wake. The handler's run
// takes the receipt; when it has run, the wake adds nothing.
func TestPRMergeHandoffSettlesTheWakeWithOneRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5241001
	secret := "che5241-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")

	firePRWebhook(t, secret, installationID, 1, identifier+": work", "", "feat/che5241a", "merged")

	wantHandedOff(t, issueID, agentID)
	tasks := issueTaskIDs(t, issueID)
	if r := prMergeReceiptFor(t, issueID, 1); r.TaskID != tasks[0] || r.Processed {
		t.Fatalf("receipt after the handoff = %+v, want pending and reserved to run %s", r, tasks[0])
	}

	finishRun(t, tasks[0])
	dispatchMergeWake(t, issueID)

	if got := issueTaskIDs(t, issueID); len(got) != 1 {
		t.Fatalf("runs after the wake = %v, want only the handler's run", got)
	}
	if r := prMergeReceiptFor(t, issueID, 1); r.TaskID != tasks[0] || !r.Processed {
		t.Fatalf("receipt after the wake = %+v, want processed with run %s", r, tasks[0])
	}
}

// A redelivery of the merge adds no receipt and no run.
func TestPRMergeRedeliveryAfterHandoffAddsNothing(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5241002
	secret := "che5241-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	firePRWebhook(t, secret, installationID, 2, identifier+": work", "", "feat/che5241b", "merged")
	wantHandedOff(t, issueID, agentID)

	firePRWebhook(t, secret, installationID, 2, identifier+": work", "", "feat/che5241b", "merged")
	triggerMergeWake(t, issueID, 2)

	if got := prMergeReceipts(t, issueID); len(got) != 1 {
		t.Fatalf("receipts after redelivery = %+v, want 1", got)
	}
	tasks := issueTaskIDs(t, issueID)
	finishRun(t, tasks[0])
	dispatchMergeWake(t, issueID)
	if got := issueTaskIDs(t, issueID); len(got) != 1 {
		t.Fatalf("runs after redelivery and wake = %v, want 1", got)
	}
}

// A child is open when the merge arrives: no handoff, and the wake captures
// its receipt while the current agent runs. When the last child is done the
// handoff reserves that receipt, so the wake adds no run after the handler's.
func TestPRMergeOpenChildThenHandoffReservesPendingReceipt(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5241003
	secret := "che5241-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	childID := prMergeOpenChild(t, issueID)
	current := busyWithCurrentRun(t, issueID, agentID)

	firePRWebhook(t, secret, installationID, 3, identifier+": work", "", "feat/che5241c", "merged")

	if status := issueStatusForTest(t, issueID); status != "in_review" {
		t.Fatalf("status = %q, want in_review while the child is open", status)
	}
	if r := prMergeReceiptFor(t, issueID, 3); r.TaskID != "" || r.Processed {
		t.Fatalf("receipt with the child open = %+v, want pending and free", r)
	}
	if n := prMergeWakeRuns(t, issueID); n != 0 {
		t.Fatalf("wake runs with the child open = %d, want 0 (deferred behind the current run)", n)
	}

	finishRun(t, current)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE id=$1`, childID); err != nil {
		t.Fatal(err)
	}
	prMergeAutoComplete(t, issueID)

	handler := issueTaskIDs(t, issueID)[1]
	if r := prMergeReceiptFor(t, issueID, 3); r.TaskID != handler || r.Processed {
		t.Fatalf("receipt after the handoff = %+v, want reserved to run %s", r, handler)
	}
	finishRun(t, handler)
	dispatchMergeWake(t, issueID)

	if got := issueTaskIDs(t, issueID); len(got) != 2 {
		t.Fatalf("runs = %v, want the current run and the handler's only", got)
	}
	if r := prMergeReceiptFor(t, issueID, 3); r.TaskID != handler || !r.Processed {
		t.Fatalf("receipt after the wake = %+v, want processed with run %s", r, handler)
	}
}

// The same sequence, but the handler's run is cancelled before it starts: the
// receipt goes back to the rule, which starts its own run once.
func TestPRMergeCancelledHandoffRunGivesTheReceiptBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5241004
	secret := "che5241-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	childID := prMergeOpenChild(t, issueID)
	current := busyWithCurrentRun(t, issueID, agentID)
	firePRWebhook(t, secret, installationID, 4, identifier+": work", "", "feat/che5241d", "merged")
	finishRun(t, current)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE id=$1`, childID); err != nil {
		t.Fatal(err)
	}
	prMergeAutoComplete(t, issueID)
	handler := issueTaskIDs(t, issueID)[1]
	if r := prMergeReceiptFor(t, issueID, 4); r.TaskID != handler {
		t.Fatalf("receipt = %+v, want reserved to run %s", r, handler)
	}

	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='cancelled', completed_at=now() WHERE id=$1`, handler); err != nil {
		t.Fatal(err)
	}
	dispatchMergeWake(t, issueID)

	if n := prMergeWakeRuns(t, issueID); n != 1 {
		t.Fatalf("wake runs after the cancelled handoff run = %d, want 1", n)
	}
	dispatchMergeWake(t, issueID)
	if n := prMergeWakeRuns(t, issueID); n != 1 {
		t.Fatalf("wake runs after a second pass = %d, want still 1", n)
	}
}

// Bridge catch-up: the wake's receipt is pending before the decision runs. The
// handoff reserves it and the issue gets one run.
func TestPRMergeCatchUpReservesExistingReceipt(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5241005
	secret := "che5241-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	firePRWebhook(t, secret, installationID, 5, identifier+": work", "", "feat/che5241e", "opened")
	if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET state='merged', merge_commit_sha='merge5' WHERE workspace_id=$1 AND pr_number=5`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	current := busyWithCurrentRun(t, issueID, agentID)
	triggerMergeWake(t, issueID, 5)
	if r := prMergeReceiptFor(t, issueID, 5); r.TaskID != "" || r.Processed {
		t.Fatalf("receipt before the catch-up = %+v, want pending and free", r)
	}
	finishRun(t, current)

	prMergeAutoComplete(t, issueID)

	handler := issueTaskIDs(t, issueID)[1]
	if r := prMergeReceiptFor(t, issueID, 5); r.TaskID != handler || r.Processed {
		t.Fatalf("receipt after the catch-up = %+v, want reserved to run %s", r, handler)
	}
	finishRun(t, handler)
	dispatchMergeWake(t, issueID)
	if got := issueTaskIDs(t, issueID); len(got) != 2 {
		t.Fatalf("runs = %v, want the current run and the handler's only", got)
	}
}

// A receipt of another PR on the same issue is not reserved; its wake runs.
func TestPRMergeHandoffLeavesOtherPRReceiptsAlone(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5241006
	secret := "che5241-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	current := busyWithCurrentRun(t, issueID, agentID)
	triggerMergeWake(t, issueID, 999)
	finishRun(t, current)

	firePRWebhook(t, secret, installationID, 6, identifier+": work", "", "feat/che5241f", "merged")

	handler := issueTaskIDs(t, issueID)[1]
	if r := prMergeReceiptFor(t, issueID, 6); r.TaskID != handler {
		t.Fatalf("receipt of the merged PR = %+v, want reserved to run %s", r, handler)
	}
	if r := prMergeReceiptFor(t, issueID, 999); r.TaskID != "" || r.Processed {
		t.Fatalf("receipt of the other PR = %+v, want untouched", r)
	}
	finishRun(t, handler)
	dispatchMergeWake(t, issueID)
	if n := prMergeWakeRuns(t, issueID); n != 1 {
		t.Fatalf("wake runs = %d, want 1 for the other PR", n)
	}
	if r := prMergeReceiptFor(t, issueID, 999); !r.Processed {
		t.Fatalf("receipt of the other PR = %+v, want processed by its own wake", r)
	}
}

// A receipt reserved to another waiting run stays with that run.
func TestPRMergeHandoffLeavesReceiptOfAnotherRunAlone(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5241007
	secret := "che5241-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	firePRWebhook(t, secret, installationID, 7, identifier+": work", "", "feat/che5241g", "opened")
	if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET state='merged', merge_commit_sha='merge7' WHERE workspace_id=$1 AND pr_number=7`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	current := busyWithCurrentRun(t, issueID, agentID)
	triggerMergeWake(t, issueID, 7)
	// Another run queued for the issue holds the receipt.
	var other string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		SELECT agent_id, runtime_id, issue_id, 'queued', 0 FROM agent_task_queue WHERE id = $1 RETURNING id::text`, current).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_wakeup_receipt SET task_id=$1 WHERE event_key='github:acme/widget#7' AND wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id=$2)`, other, issueID); err != nil {
		t.Fatal(err)
	}
	finishRun(t, current)

	prMergeAutoComplete(t, issueID)

	if r := prMergeReceiptFor(t, issueID, 7); r.TaskID != other || r.Processed {
		t.Fatalf("receipt after the handoff = %+v, want still reserved to run %s", r, other)
	}
}

// A handoff whose run is refused (the handler agent is paused) moves the
// status and reserves nothing.
func TestPRMergeHandoffRefusedRunReservesNothing(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5241008
	secret := "che5241-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prHandoffIssue(t, installationID, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	firePRWebhook(t, secret, installationID, 8, identifier+": work", "", "feat/che5241h", "opened")
	if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET state='merged', merge_commit_sha='merge8' WHERE workspace_id=$1 AND pr_number=8`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, agentID)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = NULL, scheduling_paused_by = NULL WHERE id = $1`, agentID)
	})

	prMergeAutoComplete(t, issueID)

	if status := issueStatusForTest(t, issueID); status != "merged" {
		t.Fatalf("status = %q, want merged: the move stays written", status)
	}
	if n := taskCountFor(t, issueID, agentID); n != 0 {
		t.Fatalf("runs = %d, want 0: the handoff run was refused", n)
	}
	for _, r := range prMergeReceipts(t, issueID) {
		if r.TaskID != "" {
			t.Fatalf("receipt %+v reserved by a refused handoff", r)
		}
	}
}
