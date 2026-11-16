package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// A squad owner's leader run joins the issue's tx like an agent owner's run:
// commit keeps one leader task, a capped wake or a rollback leaves none.
func TestCIWatchCreateIssue_SquadLeaderWakeAndRollback(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	dest := testHandler.CIWatchDestinations().Issue
	leaderID := createHandlerTestAgent(t, "CI Squad Leader", nil)
	squadID := insertSquad(t, ctx, testWorkspaceID, leaderID, "CI Watch Squad "+leaderID[:8])
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID) })
	create := func(wake, commit bool) string {
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := dest.CreateIssue(ctx, tx, ciwatch.IssueSpec{
			WorkspaceID: testWorkspaceID, Title: "fix(ci): squad", Body: "body", Priority: "urgent",
			AssigneeType: "squad", AssigneeID: squadID, CreatorID: testUserID, Wake: wake,
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
	leaderTasks := func(id string) (n int) {
		testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, id, leaderID).Scan(&n)
		return
	}
	if k := leaderTasks(create(true, true)); k != 1 {
		t.Fatalf("squad wake: leader tasks = %d, want 1", k)
	}
	if k := leaderTasks(create(false, true)); k != 0 {
		t.Fatalf("squad capped wake: leader tasks = %d, want 0", k)
	}
	if k := leaderTasks(create(true, false)); k != 0 {
		t.Fatalf("squad rollback: leader tasks = %d, want 0", k)
	}
}

// UpdateIssue with Wake starts the owner's run in the caller's tx: an agent owner and a
// squad leader each get one task, a second wake on the same issue is not an error, and
// no Wake leaves the issue's runs alone.
func TestCIWatchUpdateIssue_Wake(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	dest := testHandler.CIWatchDestinations().Issue
	agentID := createHandlerTestAgent(t, "CI Update Owner", nil)
	leaderID := createHandlerTestAgent(t, "CI Update Leader", nil)
	squadID := insertSquad(t, ctx, testWorkspaceID, leaderID, "CI Update Squad "+leaderID[:8])
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID) })
	mk := func(assigneeType, assigneeID string) string {
		var id string
		if err := testPool.QueryRow(ctx, `INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, assignee_type, assignee_id, number)
			VALUES ($1, 'fix(ci): low', 'todo', 'medium', 'member', $2, $3, $4, (SELECT COALESCE(max(number),0)+1 FROM issue WHERE workspace_id=$1))
			RETURNING id`, testWorkspaceID, testUserID, assigneeType, assigneeID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, id)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id)
		})
		return id
	}
	update := func(id string, wake, commit bool) {
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := dest.UpdateIssue(ctx, tx, ciwatch.IssueUpdate{WorkspaceID: testWorkspaceID, IssueID: id, Priority: "urgent", PrincipalID: testUserID, Wake: wake}); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if !commit {
			_ = tx.Rollback(ctx)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tasks := func(id, agent string) (n int) {
		testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, id, agent).Scan(&n)
		return
	}
	priority := func(id string) (p string) {
		testPool.QueryRow(ctx, `SELECT priority FROM issue WHERE id = $1`, id).Scan(&p)
		return
	}
	agentIssue := mk("agent", agentID)
	update(agentIssue, true, true)
	if k, p := tasks(agentIssue, agentID), priority(agentIssue); k != 1 || p != "urgent" {
		t.Fatalf("agent wake: tasks=%d priority=%s, want 1/urgent", k, p)
	}
	update(agentIssue, true, true) // a run is already pending: still no error, still one task
	if k := tasks(agentIssue, agentID); k != 1 {
		t.Fatalf("second agent wake: tasks=%d, want 1", k)
	}
	squadIssue := mk("squad", squadID)
	update(squadIssue, true, false)
	if k := tasks(squadIssue, leaderID); k != 0 {
		t.Fatalf("squad wake rolled back: tasks=%d, want 0", k)
	}
	update(squadIssue, true, true)
	if k := tasks(squadIssue, leaderID); k != 1 {
		t.Fatalf("squad wake: leader tasks=%d, want 1", k)
	}
	quiet := mk("agent", agentID)
	update(quiet, false, true)
	if k, p := tasks(quiet, agentID), priority(quiet); k != 0 || p != "urgent" {
		t.Fatalf("no wake: tasks=%d priority=%s, want 0/urgent", k, p)
	}
}

// The whole P3 → P2 path through the real ledger, issue destination and task queue:
// the P3 owner issue is medium and runs its owner, and the P2 escalation raises it to
// urgent and runs the owner again.
func TestCIWatchP3EscalatesToP2ThroughTheTaskPath(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "CI P3 Owner", nil)
	repo := "p3esc-" + agentID[:8]
	if _, err := testPool.Exec(ctx, `INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name, mode,
			fix_owner_kind, fix_owner_id, notify_member_id, authorized_by_member_id) VALUES ($1,'o',$2,'act','agent',$3,$4,$4)`,
		testWorkspaceID, repo, agentID, testUserID); err != nil {
		t.Fatal(err)
	}
	var issueID string
	t.Cleanup(func() {
		c := context.Background()
		if issueID != "" {
			testPool.Exec(c, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
			testPool.Exec(c, `DELETE FROM comment WHERE issue_id = $1`, issueID)
			testPool.Exec(c, `DELETE FROM issue WHERE id = $1`, issueID)
		}
		for _, tbl := range []string{"ci_watch_event", "ci_watch_action", "ci_signature_owner", "ci_watch_config"} {
			testPool.Exec(c, `DELETE FROM `+tbl+` WHERE workspace_id = $1 AND repo_name = $2`, testWorkspaceID, repo)
		}
	})
	ledger := ciwatch.NewLedger(testPool, ciwatch.NewStormRecheck(time.Now))
	dests := testHandler.CIWatchDestinations()
	dests.RedMain = sameHolderClaims{} // the production claimer is not wired yet: red-main stays Shadow-only
	ledger.RegisterStormExecutors(dests)
	admit := func(run int64, protocol string) {
		if _, err := ledger.Admit(ctx, ciwatch.Failure{WorkspaceID: testWorkspaceID, RepoOwner: "o", RepoName: repo,
			RunID: run, SignatureHash: "sigP3esc", Protocol: protocol, CheckName: "p3esc"}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			ran, err := ledger.RunOne(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !ran {
				return
			}
		}
		t.Fatal("ledger did not settle")
	}
	state := func() (priority string, tasks int) {
		if err := testPool.QueryRow(ctx, `SELECT i.id::text, i.priority FROM ci_signature_owner o JOIN issue i ON i.id = o.issue_id
			WHERE o.workspace_id = $1 AND o.repo_name = $2`, testWorkspaceID, repo).Scan(&issueID, &priority); err != nil {
			t.Fatal(err)
		}
		testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&tasks)
		return
	}
	admit(7001, "P3")
	if p, k := state(); p != "medium" || k != 1 {
		t.Fatalf("P3 issue: priority=%s tasks=%d, want medium/1", p, k)
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE issue_id = $1`, issueID); err != nil {
		t.Fatal(err)
	}
	admit(7002, "P2")
	if p, k := state(); p != "urgent" || k != 2 {
		t.Fatalf("after P2: priority=%s tasks=%d, want urgent/2", p, k)
	}
}
