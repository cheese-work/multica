package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5244: every PR event writer commits the pr_merge_owed intent with its
// source fact, before the commit, and runs one fenced inline attempt after it.
// A failed upsert rolls the source back so the sender retries; an attempt
// that never settles leaves the row for the scheduler.

const owedWriterInst int64 = 5244000

// failUpsertStarter fails the nth pr_merge_owed upsert made inside a
// transaction it started, like a database error before the commit.
type failUpsertStarter struct {
	base  txStarter
	nth   int
	calls int
}

type failUpsertTx struct {
	pgx.Tx
	s *failUpsertStarter
}

func (s *failUpsertStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.base.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failUpsertTx{Tx: tx, s: s}, nil
}

func (f failUpsertTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "-- name: UpsertPRMergeOwed ") {
		f.s.calls++
		if f.s.calls == f.s.nth {
			return errRow{err: errors.New("injected upsert failure")}
		}
	}
	return f.Tx.QueryRow(ctx, sql, args...)
}

// noAttempt is a handler copy whose inline attempt dies before it reads the
// generation: the process stopped right after the source transaction committed.
func noAttempt() *Handler {
	h := *testHandler
	h.Queries = db.New(failQueryDBTX{DBTX: testPool, failOn: "-- name: GetPRMergeOwedGeneration ", err: errors.New("injected failure")})
	return &h
}

// failingUpsert is a handler copy whose nth upsert fails.
func failingUpsert(nth int) (*Handler, *failUpsertStarter) {
	h := *testHandler
	s := &failUpsertStarter{base: testHandler.TxStarter, nth: nth}
	h.TxStarter = s
	return &h, s
}

const owedSecret = "che5244-secret"

// owedWebhook delivers one merged-PR webhook to h and returns the status.
func owedWebhook(t *testing.T, h *Handler, inst int64, number int32, title, body string) int {
	t.Helper()
	t.Setenv("GITHUB_WEBHOOK_SECRET", owedSecret)
	code, _ := postPRWebhook(t, h, owedSecret, inst, number, title, body, fmt.Sprintf("feat/che5244-%d", number), "merged")
	return code
}

// owedHandoffIssue is an in_review issue on a workflow whose merged step hands
// to an agent, in a workspace that moves issues on a merge.
func owedHandoffIssue(t *testing.T, inst int64) (issueID, identifier, agentID string) {
	t.Helper()
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	issueID, identifier, agentID = prHandoffIssue(t, inst, true)
	setWorkspacePRMergeStatusForTest(t, "merged")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issueID)
	})
	return issueID, identifier, agentID
}

func dueNow(t *testing.T, issueID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 minute' WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issueID); err != nil {
		t.Fatal(err)
	}
}

// schedulerPass is one scheduler tick, with no PR event.
func schedulerPass(t *testing.T) {
	t.Helper()
	if err := (&service.IssueWakeupService{Tasks: testHandler.TaskService}).TickWorkspaces(context.Background(), parseUUID(testWorkspaceID)); err != nil {
		t.Logf("tick: %v", err)
	}
}

func wantUnmoved(t *testing.T, issueID string) {
	t.Helper()
	if got := issueStatusForTest(t, issueID); got != "in_review" {
		t.Fatalf("status = %q, want in_review (unmoved)", got)
	}
}

func TestPRMergeOwedWriter_GitHub_InterruptedAfterCommitSchedulerMovesOnce(t *testing.T) {
	issueID, identifier, agentID := owedHandoffIssue(t, owedWriterInst+1)

	if code := owedWebhook(t, noAttempt(), owedWriterInst+1, 1, "work", "Closes "+identifier); code != http.StatusAccepted {
		t.Fatalf("webhook: %d", code)
	}
	wantUnmoved(t, issueID)
	mustOwed(t, issueID)

	dueNow(t, issueID)
	schedulerPass(t)

	wantHandedOff(t, issueID, agentID)
	if r := owedRowFor(t, issueID); r != nil {
		t.Fatalf("row must be gone after the move: %+v", r)
	}
	if n := len(statusRows(t, issueID)); n != 1 {
		t.Fatalf("status_changed rows = %d, want 1", n)
	}
}

func TestPRMergeOwedWriter_GitHub_FailedFirstUpsertRollsBackAndRedeliveryMovesOnce(t *testing.T) {
	issueID, identifier, agentID := owedHandoffIssue(t, owedWriterInst+2)
	h, _ := failingUpsert(1)

	if code := owedWebhook(t, h, owedWriterInst+2, 2, "work", "Closes "+identifier); code < 500 {
		t.Fatalf("a failed intent upsert must fail the delivery so GitHub redelivers, got %d", code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2`, testWorkspaceID); n != 0 {
		t.Fatalf("the PR mirror must roll back with the failed upsert, found %d rows", n)
	}
	if r := owedRowFor(t, issueID); r != nil {
		t.Fatalf("no intent row may survive a rolled-back transaction: %+v", r)
	}
	wantUnmoved(t, issueID)

	if code := owedWebhook(t, testHandler, owedWriterInst+2, 2, "work", "Closes "+identifier); code != http.StatusAccepted {
		t.Fatalf("redelivery: %d", code)
	}
	wantHandedOff(t, issueID, agentID)
	if r := owedRowFor(t, issueID); r != nil {
		t.Fatalf("row must be gone after the move: %+v", r)
	}
}

func TestPRMergeOwedWriter_GitHub_StopGateRefusalKeepsRow(t *testing.T) {
	issueID, identifier, agentID := owedHandoffIssue(t, owedWriterInst+3)
	otherAgent, runtimeID := mul8188Agent(t)
	// A run stopped by a merged PR holds the gate at whatever step the merge
	// moves the issue to; a superseded-head run only holds its own step.
	run := supersededRun(t, otherAgent, runtimeID, issueID, "in_review", testutil.Cols{"failure_reason": "pr_merged"})
	stopGateClosedFor(t, issueID, run)

	if code := owedWebhook(t, testHandler, owedWriterInst+3, 3, "work", "Closes "+identifier); code != http.StatusAccepted {
		t.Fatalf("webhook: %d", code)
	}
	wantUnmoved(t, issueID)
	r := mustOwed(t, issueID)
	if r.LastErr == nil || r.Fire != 1 {
		t.Fatalf("a refused move must reschedule the row with its error: %+v", r)
	}

	daemonAck(t, run)
	dueNow(t, issueID)
	schedulerPass(t)

	// The planted run belongs to the same agent, so count only the new one.
	status, assigneeType, assigneeID := issueStatusAndAssignee(t, issueID)
	if status != "merged" || assigneeType != "agent" || assigneeID != agentID {
		t.Fatalf("status=%q assignee=%s/%s, want merged handed to agent %s", status, assigneeType, assigneeID, agentID)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND id<>$2`, issueID, run); n != 1 {
		t.Fatalf("handler runs = %d, want 1", n)
	}
	if r := owedRowFor(t, issueID); r != nil {
		t.Fatalf("row must be gone after the move: %+v", r)
	}
}

func TestPRMergeOwedWriter_GitHub_FailureMidwayThroughSeveralIssuesKeepsNone(t *testing.T) {
	issueA, identA, agentA := owedHandoffIssue(t, owedWriterInst+4)
	// A second issue in the same project, so it shares the workflow and agent.
	second := prAutoCompleteTestIssue(t, "second closing issue", 0)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET project_id=(SELECT project_id FROM issue WHERE id=$2), status='in_review', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, second.ID, issueA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, second.ID)
	})
	issueB, identB, agentB := second.ID, second.Identifier, agentA
	body := "Closes " + identA + "\nCloses " + identB
	h, s := failingUpsert(2)

	if code := owedWebhook(t, h, owedWriterInst+4, 4, "work", body); code < 500 {
		t.Fatalf("the second upsert failed: the delivery must fail, got %d", code)
	}
	if s.calls < 2 {
		t.Fatalf("upserts attempted = %d, want the second to have been reached", s.calls)
	}
	for _, id := range []string{issueA, issueB} {
		if r := owedRowFor(t, id); r != nil {
			t.Fatalf("issue %s kept a row from the rolled-back transaction: %+v", id, r)
		}
		wantUnmoved(t, id)
	}

	if code := owedWebhook(t, testHandler, owedWriterInst+4, 4, "work", body); code != http.StatusAccepted {
		t.Fatalf("redelivery: %d", code)
	}
	wantHandedOff(t, issueA, agentA)
	wantHandedOff(t, issueB, agentB)
}

// The parent is owed a decision when its workflow reads child PRs, and only
// then.
func TestPRMergeOwedWriter_GitHub_ParentOwedOnlyWithIncludeChildPRs(t *testing.T) {
	for _, tc := range []struct {
		name          string
		includeChild  bool
		wantParentRow bool
	}{{"include child PRs", true, true}, {"own PRs only", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			inst := owedWriterInst + 6
			parentID, _, _ := owedHandoffIssue(t, inst)
			if _, err := testPool.Exec(context.Background(),
				`UPDATE issue_workflow SET include_child_prs=$2 WHERE id=(SELECT workflow_id FROM project WHERE id=(SELECT project_id FROM issue WHERE id=$1))`,
				parentID, tc.includeChild); err != nil {
				t.Fatal(err)
			}
			child := prAutoCompleteTestIssue(t, "owed child", 0)
			if _, err := testPool.Exec(context.Background(), `UPDATE issue SET parent_issue_id=$2 WHERE id=$1`, child.ID, parentID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, child.ID)
			})

			if code := owedWebhook(t, noAttempt(), inst, 6, "work", "Closes "+child.Identifier); code != http.StatusAccepted {
				t.Fatalf("webhook: %d", code)
			}
			mustOwed(t, child.ID)
			if got := owedRowFor(t, parentID) != nil; got != tc.wantParentRow {
				t.Fatalf("parent has a row = %v, want %v", got, tc.wantParentRow)
			}
		})
	}
}

// A redelivery of a merge already recorded is not a new event: no row.
func TestPRMergeOwedWriter_GitHub_PlainRedeliveryOwesNothing(t *testing.T) {
	issueID, identifier, _ := owedHandoffIssue(t, owedWriterInst+7)
	if code := owedWebhook(t, noAttempt(), owedWriterInst+7, 7, "work", "Closes "+identifier); code != http.StatusAccepted {
		t.Fatalf("webhook: %d", code)
	}
	testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issueID)

	if code := owedWebhook(t, noAttempt(), owedWriterInst+7, 7, "work", "Closes "+identifier); code != http.StatusAccepted {
		t.Fatalf("webhook: %d", code)
	}
	if r := owedRowFor(t, issueID); r != nil {
		t.Fatalf("a redelivery of a merged PR owes nothing: %+v", r)
	}
}

func owedVCSConn(t *testing.T) db.VcsConnection {
	t.Helper()
	connID := dbfx.Insert(t, "vcs_connection", testutil.Cols{"workspace_id": testWorkspaceID, "provider": "forgejo", "instance_url": "https://forgejo.test", "account_login": "fixture", "access_token_encrypted": "unused", "webhook_secret_encrypted": "unused"})
	t.Cleanup(func() {
		bg := context.Background()
		testPool.Exec(bg, `DELETE FROM issue_vcs_pull_request WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id=$1)`, testWorkspaceID)
		testPool.Exec(bg, `DELETE FROM vcs_pull_request WHERE workspace_id=$1`, testWorkspaceID)
	})
	return db.VcsConnection{ID: parseUUID(connID), WorkspaceID: parseUUID(testWorkspaceID), Provider: "forgejo"}
}

func owedVCSMerge(number int32, body string) vcs.PullRequestEvent {
	return vcs.PullRequestEvent{
		Action: "closed", State: "merged", RepoOwner: "fixture", RepoName: "owed", Number: number, Title: "work", Body: body,
		HTMLURL:   fmt.Sprintf("https://forgejo.test/fixture/owed/pulls/%d", number),
		CreatedAt: "2026-11-01T00:00:00Z", UpdatedAt: "2026-11-02T00:00:00Z", MergedAt: "2026-11-02T00:00:00Z",
	}
}

func TestPRMergeOwedWriter_VCS_InterruptedAfterCommitSchedulerMovesOnce(t *testing.T) {
	issueID, identifier, agentID := owedHandoffIssue(t, owedWriterInst+8)
	conn := owedVCSConn(t)

	if err := noAttempt().mirrorVCSPullRequest(context.Background(), conn, owedVCSMerge(1, "Closes "+identifier)); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	wantUnmoved(t, issueID)
	mustOwed(t, issueID)

	dueNow(t, issueID)
	schedulerPass(t)

	wantHandedOff(t, issueID, agentID)
	if r := owedRowFor(t, issueID); r != nil {
		t.Fatalf("row must be gone after the move: %+v", r)
	}
}

func TestPRMergeOwedWriter_VCS_FailedFirstUpsertRollsBackAndRedeliveryMovesOnce(t *testing.T) {
	issueID, identifier, agentID := owedHandoffIssue(t, owedWriterInst+9)
	conn := owedVCSConn(t)
	h, _ := failingUpsert(1)

	if err := h.mirrorVCSPullRequest(context.Background(), conn, owedVCSMerge(2, "Closes "+identifier)); err == nil {
		t.Fatal("a failed intent upsert must fail the delivery")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM vcs_pull_request WHERE workspace_id=$1 AND pr_number=2`, testWorkspaceID); n != 0 {
		t.Fatalf("the PR mirror must roll back, found %d rows", n)
	}
	if r := owedRowFor(t, issueID); r != nil {
		t.Fatalf("no intent row may survive a rolled-back transaction: %+v", r)
	}

	if err := testHandler.mirrorVCSPullRequest(context.Background(), conn, owedVCSMerge(2, "Closes "+identifier)); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	wantHandedOff(t, issueID, agentID)
}

// mergedUnlinkedPR mirrors a merged PR that names no issue, for a manual link.
func mergedUnlinkedPR(t *testing.T, inst int64, number int32) {
	t.Helper()
	t.Setenv("GITHUB_WEBHOOK_SECRET", owedSecret)
	if code, body := postPRWebhook(t, testHandler, owedSecret, inst, number, "Refactor helper", "", fmt.Sprintf("feat/che5244-%d", number), "merged"); code != http.StatusAccepted {
		t.Fatalf("webhook: %d %s", code, body)
	}
}

func linkOwedPR(t *testing.T, h *Handler, issueID string, number int32) *testutil.Response {
	t.Helper()
	url := fmt.Sprintf("https://github.com/acme/widget/pull/%d", number)
	req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/pull-requests", map[string]any{"url": url}), "id", issueID)
	return testutil.Call(t, h.LinkIssuePullRequest, req)
}

func TestPRMergeOwedWriter_Link_FailedUpsertStoresNoLinkThenInterruptedAttemptIsRecovered(t *testing.T) {
	issueID, _, agentID := owedHandoffIssue(t, owedWriterInst+10)
	mergedUnlinkedPR(t, owedWriterInst+10, 10)

	failing, _ := failingUpsert(1)
	if rec := linkOwedPR(t, failing, issueID, 10); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed upsert: status %d, want 500", rec.Code)
	}
	if n := linkedPRCountForTest(t, issueID); n != 0 {
		t.Fatalf("no link may be stored when the intent upsert fails, got %d", n)
	}
	if r := owedRowFor(t, issueID); r != nil {
		t.Fatalf("no row: %+v", r)
	}

	linkOwedPR(t, noAttempt(), issueID, 10).Want(http.StatusOK)
	wantUnmoved(t, issueID)
	mustOwed(t, issueID)

	dueNow(t, issueID)
	schedulerPass(t)
	wantHandedOff(t, issueID, agentID)
}

func TestPRMergeOwedWriter_Unlink_FailedUpsertKeepsLinkThenInterruptedAttemptIsRecovered(t *testing.T) {
	inst := owedWriterInst + 11
	issueID, _, agentID := owedHandoffIssue(t, inst)
	mergedUnlinkedPR(t, inst, 11)
	// A second, open PR keeps the issue where it is while it stays linked.
	t.Setenv("GITHUB_WEBHOOK_SECRET", owedSecret)
	if code, body := postPRWebhook(t, testHandler, owedSecret, inst, 12, "Other work", "", "feat/che5244-12", "opened"); code != http.StatusAccepted {
		t.Fatalf("webhook: %d %s", code, body)
	}
	linkOwedPR(t, testHandler, issueID, 12).Want(http.StatusOK)
	linkOwedPR(t, testHandler, issueID, 11).Want(http.StatusOK)
	wantUnmoved(t, issueID)
	testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issueID)

	openID := githubPRIDForTest(t, "widget", 12)
	unlink := func(h *Handler) *testutil.Response {
		req := newRequest("DELETE", "/api/issues/"+issueID+"/pull-requests/"+openID, nil)
		return testutil.Call(t, h.UnlinkIssuePullRequest, withURLParams(req, "id", issueID, "prId", openID))
	}
	failing, _ := failingUpsert(1)
	if rec := unlink(failing); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed upsert: status %d, want 500", rec.Code)
	}
	if n := linkedPRCountForTest(t, issueID); n != 2 {
		t.Fatalf("the unlink must roll back with the failed upsert, links = %d, want 2", n)
	}

	unlink(noAttempt()).Want(http.StatusOK)
	wantUnmoved(t, issueID)
	mustOwed(t, issueID)

	dueNow(t, issueID)
	schedulerPass(t)
	wantHandedOff(t, issueID, agentID)
}

// childClosed makes a child of parent terminal, which records the event the
// parent's claim transaction consumes.
func childClosed(t *testing.T, parentID string) {
	t.Helper()
	child := prAutoCompleteTestIssue(t, "owed child", 0)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET parent_issue_id=$2, status='in_progress' WHERE id=$1`, child.ID, parentID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE id=$1`, child.ID); err != nil {
		t.Fatal(err)
	}
}

func unprocessedChildEvents(t *testing.T, parentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_child_event WHERE parent_id=$1 AND processed_at IS NULL`, parentID)
}

func processChildren(t *testing.T, parentID string) error {
	t.Helper()
	return (&service.IssueWakeupService{Tasks: testHandler.TaskService}).ProcessChildEvents(context.Background(), parseUUID(parentID))
}

func TestPRMergeOwedWriter_ChildClaim_InterruptedAfterCommitSchedulerMovesParentOnce(t *testing.T) {
	parentID, _, agentID := owedHandoffIssue(t, owedWriterInst+13)
	linkMergedGitHubPRForTest(t, parentID, "owedchild")
	original := testHandler.TaskService.PRMergeOwed
	t.Cleanup(func() { testHandler.TaskService.PRMergeOwed = original })
	testHandler.TaskService.PRMergeOwed = func(context.Context, db.IssueWakeup) error { return errors.New("interrupted") }

	childClosed(t, parentID)
	if err := processChildren(t, parentID); err != nil {
		t.Fatalf("process: %v", err)
	}
	wantUnmoved(t, parentID)
	mustOwed(t, parentID)

	testHandler.TaskService.PRMergeOwed = original
	dueNow(t, parentID)
	schedulerPass(t)
	wantHandedOff(t, parentID, agentID)
	if r := owedRowFor(t, parentID); r != nil {
		t.Fatalf("row must be gone after the move: %+v", r)
	}
}

func TestPRMergeOwedWriter_ChildClaim_FailedUpsertLeavesEventUnclaimedThenRedoMovesParentOnce(t *testing.T) {
	parentID, _, agentID := owedHandoffIssue(t, owedWriterInst+14)
	linkMergedGitHubPRForTest(t, parentID, "owedchild2")
	childClosed(t, parentID)
	pending := unprocessedChildEvents(t, parentID)
	tasks := testHandler.TaskService
	original := tasks.TxStarter
	t.Cleanup(func() { tasks.TxStarter = original })
	tasks.TxStarter = &failUpsertStarter{base: original, nth: 1}

	if err := processChildren(t, parentID); err == nil {
		t.Fatal("a failed upsert must fail the claim")
	}
	if n := unprocessedChildEvents(t, parentID); n != pending {
		t.Fatalf("unprocessed child events = %d, want %d", n, pending)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_child_event WHERE parent_id=$1 AND claimed_at IS NOT NULL AND processed_at IS NULL`, parentID); n != 0 {
		t.Fatalf("the failed claim must roll back, %d events stay claimed", n)
	}
	if r := owedRowFor(t, parentID); r != nil {
		t.Fatalf("no row: %+v", r)
	}

	tasks.TxStarter = original
	if err := processChildren(t, parentID); err != nil {
		t.Fatalf("redo: %v", err)
	}
	wantHandedOff(t, parentID, agentID)
}
