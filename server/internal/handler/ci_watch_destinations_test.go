package handler

import (
	"context"
	"fmt"
	"testing"

	"github.com/multica-ai/multica/server/internal/ciwatch"
)

// A watcher digest and a class-change comment go through the destination, then through
// the real comment-trigger path with the stored suppress set. Neither may start a run,
// even with an assigned agent and a thread owner on the issue.
func TestCIWatchComments_EnqueueZeroRuns(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	dest := testHandler.CIWatchDestinations().Comment

	for i, body := range []string{
		"CI watch: 3 more failures of this signature since the last digest.",
		"CI watch: class changed P3 -> incident. ",
	} {
		assigneeID := createHandlerTestAgent(t, fmt.Sprintf("CI Assignee %d", i), nil)
		ownerID := createHandlerTestAgent(t, fmt.Sprintf("CI Thread Owner %d", i), nil)
		issueID := dbfxIssue(t, assigneeID)
		dbfxAgentRoot(t, issueID, ownerID)

		// Control: the unsuppressed trigger set for this body is not empty.
		issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
		if err != nil {
			t.Fatal(err)
		}
		triggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue, body, nil, "member", testUserID, commentTriggerComputeOptions{})
		if len(triggers) == 0 {
			t.Fatalf("control: body %q triggers no agent, test cannot discriminate", body)
		}

		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		commentID, err := dest.AddComment(ctx, tx, ciwatch.CommentSpec{WorkspaceID: testWorkspaceID, IssueID: issueID, AuthorID: testUserID, Body: body})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		comment, err := testHandler.Queries.GetComment(ctx, parseUUID(commentID))
		if err != nil {
			t.Fatal(err)
		}
		if len(comment.SuppressedAgentIds) < len(triggers) {
			t.Fatalf("stored suppress set %v does not cover trigger set of %d", comment.SuppressedAgentIds, len(triggers))
		}
		testHandler.triggerTasksForComment(ctx, issue, comment, nil, "member", testUserID, testUserID, comment.SuppressedAgentIds, commentSteerRequest{})
		tasks, err := testHandler.Queries.ListTasksByIssue(ctx, parseUUID(issueID))
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 0 {
			t.Fatalf("watcher comment %q enqueued %d runs, want 0: %+v", body, len(tasks), tasks)
		}
	}
}

func dbfxIssue(t *testing.T, assigneeID string) string {
	t.Helper()
	return createCommentTriggerPreviewIssue(t, "ci watch owner issue", "agent", assigneeID)
}

func dbfxAgentRoot(t *testing.T, issueID, ownerID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
		VALUES ($1, $2, 'agent', $3, 'I am handling this')
	`, testWorkspaceID, issueID, ownerID); err != nil {
		t.Fatal(err)
	}
}

// Issue create joins the caller's tx: a wake enqueues exactly one owner run, a capped
// wake enqueues none, and a rolled-back tx leaves neither issue nor run.
func TestCIWatchCreateIssue_WakeAndRollback(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	dest := testHandler.CIWatchDestinations().Issue
	agentID := createHandlerTestAgent(t, "CI Fix Owner", nil)
	create := func(wake, commit bool) string {
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := dest.CreateIssue(ctx, tx, ciwatch.IssueSpec{
			WorkspaceID: testWorkspaceID, Title: "fix(ci): test", Body: "body", Priority: "urgent",
			AssigneeType: "agent", AssigneeID: agentID, CreatorID: testUserID, Wake: wake,
			Metadata: map[string]any{"ci_watch_count": 1},
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if !commit {
			_ = tx.Rollback(ctx)
			return id
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, id)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id)
		})
		return id
	}
	count := func(id string) (issues, tasks int) {
		testPool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE id = $1`, id).Scan(&issues)
		testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, id).Scan(&tasks)
		return
	}
	if i, k := count(create(true, true)); i != 1 || k != 1 {
		t.Fatalf("wake: issues=%d tasks=%d, want 1/1", i, k)
	}
	if i, k := count(create(false, true)); i != 1 || k != 0 {
		t.Fatalf("capped wake: issues=%d tasks=%d, want 1/0", i, k)
	}
	if i, k := count(create(true, false)); i != 0 || k != 0 {
		t.Fatalf("rollback: issues=%d tasks=%d, want 0/0", i, k)
	}
	var count1 string
	id := create(false, true)
	testPool.QueryRow(ctx, `SELECT metadata->>'ci_watch_count' FROM issue WHERE id=$1`, id).Scan(&count1)
	if count1 != "1" {
		t.Fatalf("metadata ci_watch_count = %q, want 1", count1)
	}
}
