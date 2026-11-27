package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4906: a handoff into a review step whose reviewer already holds its WIP
// limit of review runs creates no run (the stage queue refuses it and the
// scheduler starts it later). The timeline row must say so, never run=true.

// reviewerFull gives the ready agent `n` review-stage PRs that each hold a
// running run, older than any PR created in the test.
func reviewerFull(t *testing.T, agentID string, n int) {
	t.Helper()
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	for i := 0; i < n; i++ {
		issueID := dbfx.Issue(t, "stage slot holder", testutil.Cols{
			"status": "agent_review", "assignee_type": "agent", "assignee_id": agentID,
		})
		linkOpenPR(t, issueID, "now() - interval '2 hours'")
		dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running"})
	}
}

func linkOpenPR(t *testing.T, issueID, createdAt string) {
	t.Helper()
	prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "multica",
		"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "stage PR", "state": "open",
		"html_url": "https://example.test/pr", "pr_created_at": testutil.Raw(createdAt), "pr_updated_at": testutil.Raw("now()"),
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issueID, prID)
	t.Cleanup(func() { testPool.Exec(t.Context(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID) })
}

func reviewHandoffIssue(t *testing.T, agentID string) string {
	t.Helper()
	createTestCustomStatus(t, "agent_review", "started")
	wf := createTestWorkflowSteps(t, "Stage queue handoff "+workflowTestSuffix(), []map[string]any{
		{"status_key": "in_progress"},
		{"status_key": "agent_review"},
		{"status_key": "in_review"},
		{"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "Stage queue handoff project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, projectID, "agent_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "stage queue handoff", testutil.Cols{"project_id": projectID, "status": "in_progress"})
	linkOpenPR(t, issueID, "now()")
	return issueID
}

func taskCount(t *testing.T, issueID string) (n int) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&n)
	return n
}

func TestHandoffIntoFullReviewStageRecordsNoRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := reviewHandoffIssue(t, agentID)
	ids := recordActivityIDs(t, issueID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK)

	if n := taskCount(t, issueID); n != 0 {
		t.Fatalf("tasks on the issue = %d, want 0: the stage queue refuses the run", n)
	}
	row := wantOneRecordedRow(t, issueID, "ui_or_api", ids())
	if row["handoff_run"] == "true" {
		t.Fatalf("handoff_run = true although no run exists; details = %v", row)
	}
	if row["handoff_run_reason"] != "stage_queue_full" {
		t.Fatalf("handoff_run_reason = %v, want stage_queue_full; details = %v", row["handoff_run_reason"], row)
	}
}

// With a free slot the same handoff starts its run and says so.
func TestHandoffIntoReviewStageWithFreeSlotRecordsRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 1)
	issueID := reviewHandoffIssue(t, agentID)
	ids := recordActivityIDs(t, issueID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK)

	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks on the issue = %d, want 1", n)
	}
	row := wantOneRecordedRow(t, issueID, "ui_or_api", ids())
	if row["handoff_run"] != "true" || row["handoff_run_reason"] == "stage_queue_full" {
		t.Fatalf("handoff details = %v, want run=true", row)
	}
}

// CHE-9722: a refused review handoff is queued, and starts when a slot frees
// with no manual wakeup.
func TestRefusedReviewHandoffStartsWhenSlotFrees(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := reviewHandoffIssue(t, agentID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK)
	if n := taskCount(t, issueID); n != 0 {
		t.Fatalf("tasks before a slot frees = %d, want 0", n)
	}

	// Still full: the release tick starts nothing.
	if err := testHandler.TaskService.ReleaseStageQueues(t.Context()); err != nil {
		t.Fatalf("release (full): %v", err)
	}
	if n := taskCount(t, issueID); n != 0 {
		t.Fatalf("tasks while the stage is full = %d, want 0", n)
	}

	// A slot frees: one holder's run completes.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now()
	  WHERE id = (SELECT t.id FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
	               WHERE t.agent_id = $1 AND t.status = 'running' AND i.title = 'stage slot holder' LIMIT 1)`, agentID)

	if err := testHandler.TaskService.ReleaseStageQueues(t.Context()); err != nil {
		t.Fatalf("release (free): %v", err)
	}
	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks after a slot frees = %d, want 1: the refused handoff must start without a manual wakeup", n)
	}
}

// CHE-9722: a review handoff the loop breaker refuses is queued, not blocked.
// The issue stays in agent_review with a notice on it, and the run starts on
// its own once the breaker's window clears. While the window is still full the
// release tick starts nothing and posts no second notice.
func TestLoopBreakerRefusedReviewHandoffIsQueuedAndRetried(t *testing.T) {
	seedTestCatalog(t)
	t.Setenv("MULTICA_LOOP_BREAKER_MAX_RUNS", "6")
	agentID := seededReadyAgentID(t)
	issueID := reviewHandoffIssue(t, agentID)
	for i := 0; i < 5; i++ {
		at := time.Now().Add(-10 * time.Minute).Add(time.Duration(i) * time.Second)
		dbfx.Task(t, agentID, testutil.Cols{
			"issue_id": issueID, "status": "completed", "created_at": at, "started_at": at, "completed_at": at,
			"trigger_evidence_kind": "comment",
		})
	}
	before := taskCount(t, issueID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK)
	service.WaitRunBudgetHooks()

	if got := taskCount(t, issueID); got != before {
		t.Fatalf("tasks = %d, want %d: the breaker refuses the run", got, before)
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
	if status != "agent_review" {
		t.Fatalf("issue is %s, want agent_review: a queued review run must not block the issue", status)
	}
	notices := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND author_type='system' AND content LIKE '%Loop breaker%queued%'`, issueID)
	}
	if n := notices(); n != 1 {
		t.Fatalf("loop-breaker notices = %d, want 1 on the issue", n)
	}

	// Window still full: the release tick retries quietly and starts nothing.
	if err := testHandler.TaskService.ReleaseStageQueues(t.Context()); err != nil {
		t.Fatalf("release (window full): %v", err)
	}
	service.WaitRunBudgetHooks()
	if got := taskCount(t, issueID); got != before {
		t.Fatalf("tasks while the window is full = %d, want %d", got, before)
	}
	if n := notices(); n != 1 {
		t.Fatalf("loop-breaker notices after a retry = %d, want still 1", n)
	}

	// The window clears: the next tick starts the run, with no manual wakeup.
	dbfx.Exec(t, `UPDATE agent_task_queue SET created_at = now() - interval '2 hours' WHERE issue_id = $1`, issueID)
	if err := testHandler.TaskService.ReleaseStageQueues(t.Context()); err != nil {
		t.Fatalf("release (window clear): %v", err)
	}
	if got := taskCount(t, issueID); got != before+1 {
		t.Fatalf("tasks after the window clears = %d, want %d", got, before+1)
	}
}

// CHE-9895: the write that enters agent_review, or names the reviewer again at
// agent_review, never ends with no run and no word to the caller.

type outcomesResp struct {
	TriggerOutcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
}

func reassignSameReviewer(t *testing.T, issueID, agentID string) outcomesResp {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{
		"assignee_type": "agent", "assignee_id": agentID,
	}), "id", issueID)
	var out outcomesResp
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK).JSON(&out)
	return out
}

func wantOutcome(t *testing.T, got outcomesResp, agentID string, status DispatchStatus, reason DispatchReasonCode) {
	t.Helper()
	for _, o := range got.TriggerOutcomes {
		if o.TargetID == agentID && o.Status == status && o.ReasonCode == reason {
			return
		}
	}
	t.Fatalf("trigger_outcomes = %+v, want agent %s %s/%s", got.TriggerOutcomes, agentID, status, reason)
}

// A handoff the stage queue holds back tells the caller it is queued.
func TestRefusedReviewHandoffTellsTheCaller(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := reviewHandoffIssue(t, agentID)

	var out outcomesResp
	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK).JSON(&out)

	wantOutcome(t, out, agentID, DispatchDeferred, ReasonStageQueueFull)
}

// The reviewer is already assigned and no run exists: naming the same reviewer
// again starts exactly one review run; a repeat adds none and says it is covered.
func TestReassignSameReviewerAtAgentReviewStartsOneRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID := reviewHandoffIssue(t, agentID)
	dbfx.Exec(t, `UPDATE issue SET status = 'agent_review', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agentID)

	reassignSameReviewer(t, issueID, agentID)
	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks after re-assigning the same reviewer = %d, want 1", n)
	}
	out := reassignSameReviewer(t, issueID, agentID)
	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks after a second re-assign = %d, want still 1", n)
	}
	wantOutcome(t, out, agentID, DispatchCoalesced, ReasonCoalesced)
}

// With the stage full, the same-reviewer write queues the PR and says so; the
// run starts when a slot frees.
func TestReassignSameReviewerWhileStageFullIsQueued(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := reviewHandoffIssue(t, agentID)
	dbfx.Exec(t, `UPDATE issue SET status = 'agent_review', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agentID)

	out := reassignSameReviewer(t, issueID, agentID)
	if n := taskCount(t, issueID); n != 0 {
		t.Fatalf("tasks while the stage is full = %d, want 0", n)
	}
	wantOutcome(t, out, agentID, DispatchDeferred, ReasonStageQueueFull)

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now()
	  WHERE id = (SELECT t.id FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
	               WHERE t.agent_id = $1 AND t.status = 'running' AND i.title = 'stage slot holder' LIMIT 1)`, agentID)
	if err := testHandler.TaskService.ReleaseStageQueues(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks after a slot frees = %d, want 1", n)
	}
}

func waitingRows(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM stage_queue_waiting WHERE issue_id = $1`, issueID)
}

func freeOneReviewSlot(t *testing.T, agentID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now()
	  WHERE id = (SELECT t.id FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
	               WHERE t.agent_id = $1 AND t.status = 'running' AND i.title = 'stage slot holder' LIMIT 1)`, agentID)
}

// CHE-9895 recovery: the re-trigger the CLI names (assign the reviewer again)
// respects the stage limit and joins the queued entry. While the stage is full
// it adds no run and no second entry; once a slot frees it starts one run, and
// the release tick that finds the entry afterwards adds none.
func TestRecoveryReassignRespectsStageLimitAndNeverDuplicates(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := reviewHandoffIssue(t, agentID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK)
	if n := waitingRows(t, issueID); n != 1 {
		t.Fatalf("waiting entries after the deferred handoff = %d, want 1", n)
	}

	// Recovery while the stage is full: still deferred, no run, one entry.
	for i := 0; i < 2; i++ {
		out := reassignSameReviewer(t, issueID, agentID)
		wantOutcome(t, out, agentID, DispatchDeferred, ReasonStageQueueFull)
		if n := taskCount(t, issueID); n != 0 {
			t.Fatalf("tasks after recovery %d while full = %d, want 0: recovery must respect the limit", i+1, n)
		}
		if n := waitingRows(t, issueID); n != 1 {
			t.Fatalf("waiting entries after recovery %d = %d, want 1: recovery must join the entry", i+1, n)
		}
	}

	// A slot frees; recovery starts exactly one run.
	freeOneReviewSlot(t, agentID)
	out := reassignSameReviewer(t, issueID, agentID)
	wantOutcome(t, out, agentID, DispatchQueued, ReasonQueued)
	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks after recovery with a free slot = %d, want 1", n)
	}

	// The release tick sees the original entry and the run it became: no second run.
	if err := testHandler.TaskService.ReleaseStageQueues(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks after the release tick = %d, want 1: the entry must not duplicate the run", n)
	}
}

// CHE-9895 case 1: the issue already has the reviewer assigned when it enters
// agent_review. With a free slot the move starts exactly one run.
func TestEnterAgentReviewWithReviewerAlreadyAssignedStartsOneRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID := reviewHandoffIssue(t, agentID)
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agentID)

	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK)
	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks after entering agent_review with the reviewer assigned = %d, want 1", n)
	}
}

// Same entry with the stage full: no run, one queued entry, and the caller is told.
func TestEnterAgentReviewWithReviewerAlreadyAssignedWhileFullIsQueued(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := reviewHandoffIssue(t, agentID)
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agentID)

	var out outcomesResp
	testutil.Call(t, testHandler.MoveIssue, moveStatus(issueID, "agent_review")).Want(http.StatusOK).JSON(&out)
	wantOutcome(t, out, agentID, DispatchDeferred, ReasonStageQueueFull)
	if n := taskCount(t, issueID); n != 0 {
		t.Fatalf("tasks while the stage is full = %d, want 0", n)
	}
	if n := waitingRows(t, issueID); n != 1 {
		t.Fatalf("waiting entries = %d, want 1", n)
	}

	freeOneReviewSlot(t, agentID)
	if err := testHandler.TaskService.ReleaseStageQueues(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("tasks after a slot frees = %d, want 1", n)
	}
}
