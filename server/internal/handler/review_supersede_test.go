package handler

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgtype"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const (
	reviewHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reviewHeadB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reviewHeadC = "cccccccccccccccccccccccccccccccccccccccc"
)

type reviewFixture struct {
	agentID, runtimeID, issueID, prID string
	review                            string // dispatched review run for reviewHeadA
}

// reviewIssue plants an issue at its review step, a linked open PR at head A,
// and the review run dispatched for that head.
func reviewIssue(t *testing.T) reviewFixture {
	t.Helper()
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	projectID := createWorkflowTestProject(t, "Review supersede project")
	setColumnHandoff(t, projectID, "in_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "review supersede", testutil.Cols{
		"project_id": projectID, "status": "in_review", "assignee_type": "agent", "assignee_id": agentID,
	})
	prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "multica",
		"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "review PR", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"), "head_sha": reviewHeadA,
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issueID, prID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID)
	})
	review := reviewRun(t, agentID, runtimeID, issueID, prID, "in_review", reviewHeadA)
	return reviewFixture{agentID, runtimeID, issueID, prID, review}
}

func reviewRun(t *testing.T, agentID, runtimeID, issueID, prID, step, head string) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "workflow_step": step,
		"pull_request_id": prID, "context": testutil.Raw(fmt.Sprintf(`'{"head_sha":%q}'::jsonb`, head)),
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
}

func pushHead(t *testing.T, prID, head string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha = $2, pr_updated_at = now() WHERE id = $1`, prID, head)
	testHandler.supersedeStaleReviews(context.Background(), parseUUID(prID))
}

type queuedRun struct{ id, head, step string }

func queuedRuns(t *testing.T, issueID string) []queuedRun {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id::text, COALESCE(context->>'head_sha', ''), COALESCE(workflow_step, '')
		FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []queuedRun
	for rows.Next() {
		var q queuedRun
		if err := rows.Scan(&q.id, &q.head, &q.step); err != nil {
			t.Fatal(err)
		}
		out = append(out, q)
	}
	return out
}

func failureReason(t *testing.T, taskID string) string {
	t.Helper()
	var r *string
	dbfx.QueryRow(t, `SELECT failure_reason FROM agent_task_queue WHERE id = $1`, taskID).Scan(&r)
	if r == nil {
		return ""
	}
	return *r
}

// A new head cancels the review for the old one through the stop gate. The
// replacement waits for the daemon's ack and then reviews the new head.
func TestReviewSupersede_NewHeadQueuesOneReplacementAfterAck(t *testing.T) {
	f := reviewIssue(t)

	pushHead(t, f.prID, reviewHeadB)

	if got := taskStatus(t, f.review); got != "cancelled" {
		t.Fatalf("old review status = %q, want cancelled", got)
	}
	if got := failureReason(t, f.review); got != service.FailureReasonSupersededHead {
		t.Fatalf("old review failure_reason = %q, want %q", got, service.FailureReasonSupersededHead)
	}
	if q := queuedRuns(t, f.issueID); len(q) != 0 {
		t.Fatalf("replacement queued before the ack: %+v", q)
	}
	stopGateClosedFor(t, f.issueID, f.review)

	daemonAck(t, f.review)

	q := queuedRuns(t, f.issueID)
	if len(q) != 1 || q[0].head != reviewHeadB || q[0].step != "in_review" {
		t.Fatalf("queued after ack = %+v, want one in_review run for head B", q)
	}
}

// A push while the old run drains does not add a second replacement: the one
// that will be released reads the newest head.
func TestReviewSupersede_PushWhileDrainingKeepsOneReplacementWithNewHead(t *testing.T) {
	f := reviewIssue(t)

	pushHead(t, f.prID, reviewHeadB)
	pushHead(t, f.prID, reviewHeadC)

	if q := queuedRuns(t, f.issueID); len(q) != 0 {
		t.Fatalf("replacement queued while draining: %+v", q)
	}

	daemonAck(t, f.review)
	daemonAck(t, f.review) // a replayed ack must not add another

	q := queuedRuns(t, f.issueID)
	if len(q) != 1 || q[0].head != reviewHeadC {
		t.Fatalf("queued after ack = %+v, want one run for head C", q)
	}
}

// The old review may still finish after it was superseded. Its verdict is for
// a commit that is no longer the head, so the status move is refused and the
// timeline says why.
func TestReviewSupersede_LateFinishOfOldReviewIsRefusedAsStale(t *testing.T) {
	f := reviewIssue(t)
	pushHead(t, f.prID, reviewHeadB)

	body := agentUpdateIssue(t, f.issueID, f.agentID, f.review, map[string]any{"status": "blocked"}).Want(http.StatusConflict).Map()
	if body["code"] != "review_verdict_stale" {
		t.Fatalf("refusal = %v", body)
	}
	if status, _, _ := issueStatusAndAssignee(t, f.issueID); status != "in_review" {
		t.Fatalf("issue status after refused verdict = %q, want in_review", status)
	}
	var details string
	dbfx.QueryRow(t, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'review_verdict_stale'`, f.issueID).Scan(&details)
	if want := "verdict for " + reviewHeadA + " is stale"; !strings.Contains(details, want) {
		t.Fatalf("activity details %q lack %q", details, want)
	}
}

// Only review runs are superseded. The author's run on the same PR keeps going.
func TestReviewSupersede_AuthorRunIsNotCancelled(t *testing.T) {
	f := reviewIssue(t)
	author := reviewRun(t, f.agentID, f.runtimeID, f.issueID, f.prID, "in_progress", reviewHeadA)

	pushHead(t, f.prID, reviewHeadB)

	if got := taskStatus(t, author); got != "running" {
		t.Fatalf("author run status = %q, want running", got)
	}
	if got := taskStatus(t, f.review); got != "cancelled" {
		t.Fatalf("review run status = %q, want cancelled", got)
	}
}

// An author's run is queued while the issue is in progress, so it is never a
// review. A normal handoff to in_review advances its workflow step but must not
// make it cancellable by a new head.
func TestReviewSupersede_AuthorQueuedInProgressSurvivesHandoffAndNewHead(t *testing.T) {
	f := reviewIssue(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, f.review)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, f.issueID)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(f.issueID))
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.TaskService.EnqueueTaskForIssueWithHandoff(context.Background(), issue, "", pgtype.UUID{})
	if err != nil {
		t.Fatal(err)
	}
	author := uuidToString(task.ID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', dispatched_at = now(), started_at = now() WHERE id = $1`, author)

	agentUpdateIssue(t, f.issueID, f.agentID, author, map[string]any{"status": "in_review"}).Want(http.StatusOK)
	pushHead(t, f.prID, reviewHeadB)

	if got := taskStatus(t, author); got != "running" {
		t.Fatalf("author run status after handoff and new head = %q, want running", got)
	}
}

// The replacement reviews the PR its group belongs to, not whichever linked PR
// was updated last.
func TestReviewSupersede_ReplacementStaysBoundToItsPR(t *testing.T) {
	f := reviewIssue(t)
	pushHead(t, f.prID, reviewHeadB)
	siblingID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "multica",
		"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "newer sibling PR", "state": "open", "html_url": "https://example.test/sibling-pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now() + interval '1 second'"), "head_sha": reviewHeadC,
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, f.issueID, siblingID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, siblingID)
	})

	daemonAck(t, f.review)

	q := queuedRuns(t, f.issueID)
	if len(q) != 1 || q[0].head != reviewHeadB {
		t.Fatalf("replacement = %+v, want one run for the superseded PR's head %s", q, reviewHeadB)
	}
}

// A cleanup-unconfirmed report rewrites the cancelled review to failed. The b1
// trigger keeps it in the stop gate; it must stay a superseded review too, so a
// confirmed ack still releases the one replacement.
func TestReviewSupersede_CleanupUnconfirmedRewriteStillReleasesReplacement(t *testing.T) {
	f := reviewIssue(t)
	pushHead(t, f.prID, reviewHeadB)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', failure_reason = 'cleanup_unconfirmed' WHERE id = $1`, f.review)

	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent_task_queue WHERE id = $1`, f.review).Scan(&runtimeID)
	ackCancelled(t, f.review, map[string]any{"cancel_ack_proof": auth.CancelAckProof(f.review, runtimeID), "process_cleanup_confirmed": true})

	q := queuedRuns(t, f.issueID)
	if len(q) != 1 || q[0].head != reviewHeadB {
		t.Fatalf("queued after confirmed ack = %+v, want one run for head %s", q, reviewHeadB)
	}
}

// The same rewrite must not let the old review's verdict through.
func TestReviewSupersede_StaleVerdictRefusedAfterCleanupUnconfirmedRewrite(t *testing.T) {
	f := reviewIssue(t)
	pushHead(t, f.prID, reviewHeadB)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed', failure_reason = 'cleanup_unconfirmed' WHERE id = $1`, f.review)

	body := agentUpdateIssue(t, f.issueID, f.agentID, f.review, map[string]any{"status": "blocked"}).Want(http.StatusConflict).Map()
	if body["code"] != "review_verdict_stale" {
		t.Fatalf("refusal = %v", body)
	}
	if status, _, _ := issueStatusAndAssignee(t, f.issueID); status != "in_review" {
		t.Fatalf("issue status after refused verdict = %q, want in_review", status)
	}
}

// Two linked PRs can share a head SHA. The replacement keeps the identity of the
// PR it replaces, so a later push to that PR still supersedes it.
func TestReviewSupersede_ReplacementKeepsItsPRWhenASiblingSharesTheHead(t *testing.T) {
	f := reviewIssue(t)
	siblingID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "multica",
		"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "sibling PR on the same head", "state": "open", "html_url": "https://example.test/sibling-pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"), "head_sha": reviewHeadB,
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, f.issueID, siblingID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, siblingID)
	})

	pushHead(t, f.prID, reviewHeadB)
	dbfx.Exec(t, `UPDATE github_pull_request SET pr_updated_at = now() + interval '1 second' WHERE id = $1`, siblingID) // newest PR on head B
	daemonAck(t, f.review)

	q := queuedRuns(t, f.issueID)
	if len(q) != 1 {
		t.Fatalf("queued after ack = %+v, want one replacement", q)
	}
	var boundPR string
	dbfx.QueryRow(t, `SELECT pull_request_id::text FROM agent_task_queue WHERE id = $1`, q[0].id).Scan(&boundPR)
	if boundPR != f.prID {
		t.Fatalf("replacement bound to PR %s, want %s", boundPR, f.prID)
	}

	pushHead(t, f.prID, reviewHeadC)
	if got := taskStatus(t, q[0].id); got != "cancelled" {
		t.Fatalf("replacement status after the PR advanced again = %q, want cancelled", got)
	}
}
