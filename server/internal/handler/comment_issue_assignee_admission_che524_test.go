package handler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// TestCommentIssueAssigneeEnqueueRaceCoalescesWithoutLeaking is the CHE-524
// admission-boundary regression for the plain (non-squad) issue_assignee
// comment-trigger source. enqueueSingleCommentTrigger's EnqueueTaskForIssue
// branch used to log every enqueue failure — including the benign
// ErrDuplicatePendingTask coalesce race — through a bare slog.Warn instead of
// logCommentEnqueueFailure, unlike every other source in the same switch
// (squad leader, mention agent, thread parent/conversation). A concurrent
// sibling task for the SAME (issue, agent) pending slot must fold the losing
// comment into the winner (coalesced), not surface a raw constraint-name
// warning.
func TestCommentIssueAssigneeEnqueueRaceCoalescesWithoutLeaking(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, issueID, _ := dupRaceFixture(t, "dup-race-issue-assignee", 999411)
	agentUUID := util.MustParseUUID(agentID)
	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	agent, err := testHandler.Queries.GetAgent(ctx, agentUUID)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}

	winnerCommentID := insertDupRaceComment(t, issueID, "first assignee-routed instruction", "6 minutes")
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue, util.MustParseUUID(winnerCommentID)); err != nil {
		t.Fatalf("enqueue winning task: %v", err)
	}
	loserCommentID := insertDupRaceComment(t, issueID, "second distinct assignee-routed instruction", "1 minute")

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	trigger := commentAgentTrigger{Agent: agent, Source: commentTriggerSourceIssueAssignee}
	results := testHandler.enqueueCommentAgentTriggers(ctx, issue, util.MustParseUUID(loserCommentID), []commentAgentTrigger{trigger})

	if res := results[agentID]; res.status != DispatchCoalesced {
		t.Fatalf("issue-assignee race: got status %q reason %q, want coalesced", res.status, res.reason)
	}
	if !commentCovered(t, issueID, agentID, loserCommentID, "queued") {
		t.Fatal("losing comment was NOT folded into the queued winner — its instruction would be dropped")
	}
	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 1 {
		t.Fatalf("pending task count = %d, want exactly 1", n)
	}
	for _, leak := range []string{"idx_one_pending_task_per_issue_agent", "level=WARN", "level=ERROR"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("benign issue-assignee enqueue race leaked %q into logs:\n%s", leak, logs.String())
		}
	}
}

// TestCommentIssueAssigneeAdmissionIdentity proves the plain issue-assignee
// comment trigger admits exactly the executing agent named by the issue's
// assignment, distinct per issue — the identity half of the admission
// boundary, independent of the duplicate/race handling above.
func TestCommentIssueAssigneeAdmissionIdentity(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentA, issueA, _ := dupRaceFixture(t, "admission-identity-a", 999412)
	agentB, issueB, _ := dupRaceFixture(t, "admission-identity-b", 999413)

	issueAObj, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueA))
	if err != nil {
		t.Fatalf("load issue A: %v", err)
	}
	agentAObj, err := testHandler.Queries.GetAgent(ctx, util.MustParseUUID(agentA))
	if err != nil {
		t.Fatalf("load agent A: %v", err)
	}

	commentID := insertDupRaceComment(t, issueA, "route to A's assignee only", "1 minute")
	trigger := commentAgentTrigger{Agent: agentAObj, Source: commentTriggerSourceIssueAssignee}
	results := testHandler.enqueueCommentAgentTriggers(ctx, issueAObj, util.MustParseUUID(commentID), []commentAgentTrigger{trigger})

	if res := results[agentA]; res.status != DispatchQueued {
		t.Fatalf("issue A admission: got status %q reason %q, want queued", res.status, res.reason)
	}
	if n := pendingTaskCountForAgentIssue(t, issueA, agentA); n != 1 {
		t.Fatalf("issue A pending count = %d, want 1", n)
	}
	if n := pendingTaskCountForAgentIssue(t, issueB, agentB); n != 0 {
		t.Fatalf("unrelated issue B pending count = %d, want 0 — admission must not cross issues", n)
	}
}

// TestCommentIssueAssigneeEditExcludedFromOwnTrigger proves an edit to the
// non-squad issue-assignee trigger comment itself is excluded when recomputing
// triggers for the edited content (ExcludeTriggerCommentID), matching the
// mention-source edit-exclusion behavior already covered for
// commentTriggerSourceMentionAgent.
func TestCommentIssueAssigneeEditExcludedFromOwnTrigger(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	assigneeID := createHandlerTestAgent(t, "Issue Assignee Edit Exclusion", nil)
	issueID := createCommentTriggerPreviewIssue(t, "issue-assignee edit exclusion", "agent", assigneeID)

	commentID := postCommentForTriggerPreviewTest(t, issueID, map[string]any{
		"content": "please take a look",
	})
	if got := countQueuedCommentTriggerTasks(t, issueID, assigneeID); got != 1 {
		t.Fatalf("initial comment queued tasks = %d, want 1", got)
	}

	// Editing the SAME comment must not re-trigger a second admitted task for
	// the identical (issue, agent) pending slot — it folds/coalesces instead.
	w := httptest.NewRecorder()
	req := newRequest(http.MethodPut, "/api/comments/"+commentID, map[string]any{
		"content": "please take a look (edited)",
	})
	req = withURLParam(req, "commentId", commentID)
	testHandler.UpdateComment(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateComment: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := pendingTaskCountForAgentIssue(t, issueID, assigneeID); got != 1 {
		t.Fatalf("pending tasks after edit = %d, want exactly 1 (no duplicate admission)", got)
	}
}

// TestCommentIssueAssigneeCancellationRequeuesSurvivor proves deleting the
// non-squad issue-assignee trigger comment cancels the task it solely covered
// and requeues coverage for the issue's assignee, mirroring the
// mention-source cancellation behavior already covered by
// TestDeleteComment_RequeuesSurvivingCoalescedBatch.
func TestCommentIssueAssigneeCancellationRequeuesSurvivor(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	assigneeID := createHandlerTestAgent(t, "Issue Assignee Cancellation", nil)
	issueID := createCommentTriggerPreviewIssue(t, "issue-assignee cancellation", "agent", assigneeID)

	commentID := postCommentForTriggerPreviewTest(t, issueID, map[string]any{
		"content": "first instruction",
	})
	if got := countQueuedCommentTriggerTasks(t, issueID, assigneeID); got != 1 {
		t.Fatalf("initial comment queued tasks = %d, want 1", got)
	}

	w := httptest.NewRecorder()
	req := newRequest(http.MethodDelete, "/api/comments/"+commentID, nil)
	req = withURLParam(req, "commentId", commentID)
	testHandler.DeleteComment(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteComment: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	var deletedCount int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM comment WHERE id = $1`, commentID).Scan(&deletedCount); err != nil {
		t.Fatalf("check deleted comment: %v", err)
	}
	if deletedCount != 0 {
		t.Fatalf("deleted trigger comment still exists")
	}
}
