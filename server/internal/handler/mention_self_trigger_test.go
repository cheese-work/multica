package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// enqueueMentionedAgentTasksForTest mirrors the production comment path for
// @mention triggers: compute the cascade trigger set, then enqueue it. Kept as a
// test helper so these integration tests keep asserting enqueue side effects.
func enqueueMentionedAgentTasksForTest(t *testing.T, ctx context.Context, issue db.Issue, comment db.Comment, parentComment *db.Comment, authorType, authorID string) {
	t.Helper()
	triggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue, comment.Content, parentComment, authorType, authorID, commentTriggerComputeOptions{})
	testHandler.enqueueCommentAgentTriggers(ctx, issue, comment.ID, triggers)
}

type selfMentionFixture struct {
	JID        string
	RuntimeID  string
	IssueAID   string // primary issue (used for same-issue scenarios)
	IssueA     db.Issue
	IssueBID   string // a second issue (used for the cross-issue scenario)
	IssueB     db.Issue
	CommentAID string // a comment on IssueA authored by J — used as the trigger
	CommentA   db.Comment
	CommentBID string // a comment on IssueB authored by J — used as the trigger
	CommentB   db.Comment
}

func newSelfMentionFixture(t *testing.T) selfMentionFixture {
	t.Helper()
	ctx := context.Background()

	// Reuse the seeded workspace-visible agent — it already has a runtime.
	var jID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&jID); err != nil {
		t.Fatalf("load seeded agent: %v", err)
	}
	var runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, jID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}

	insertIssue := func(title string) string {
		t.Helper()
		// Pick the next per-workspace issue number; without it both inserts
		// land on the default number=0 and trip uq_issue_workspace_number.
		var number int
		if err := testPool.QueryRow(ctx, `
			UPDATE workspace
			SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
			WHERE id = $1 RETURNING issue_counter
		`, testWorkspaceID).Scan(&number); err != nil {
			t.Fatalf("next issue number: %v", err)
		}
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, creator_type, creator_id, title, assignee_type, assignee_id, number)
			VALUES ($1, 'member', $2, $3, 'agent', $4, $5)
			RETURNING id
		`, testWorkspaceID, testUserID, title, jID, number).Scan(&id); err != nil {
			t.Fatalf("create issue %q: %v", title, err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, id)
			testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, id)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id)
		})
		return id
	}

	insertJComment := func(issueID, content string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
			VALUES ($1, $2, 'agent', $3, $4)
			RETURNING id
		`, testWorkspaceID, issueID, jID, content).Scan(&id); err != nil {
			t.Fatalf("create comment on %s: %v", issueID, err)
		}
		return id
	}

	issueAID := insertIssue("self-mention test A (same-issue scenarios)")
	issueBID := insertIssue("self-mention test B (parent issue, cross-issue handoff)")

	commentAID := insertJComment(issueAID, "[@J](mention://agent/"+jID+") follow-up coming")
	commentBID := insertJComment(issueBID, "Child issue done — [@J](mention://agent/"+jID+") please wrap up here")

	issueA, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueAID))
	if err != nil {
		t.Fatalf("load issueA: %v", err)
	}
	issueB, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueBID))
	if err != nil {
		t.Fatalf("load issueB: %v", err)
	}
	commentA, err := testHandler.Queries.GetComment(ctx, util.MustParseUUID(commentAID))
	if err != nil {
		t.Fatalf("load commentA: %v", err)
	}
	commentB, err := testHandler.Queries.GetComment(ctx, util.MustParseUUID(commentBID))
	if err != nil {
		t.Fatalf("load commentB: %v", err)
	}

	return selfMentionFixture{
		JID:        jID,
		RuntimeID:  runtimeID,
		IssueAID:   issueAID,
		IssueA:     issueA,
		IssueBID:   issueBID,
		IssueB:     issueB,
		CommentAID: commentAID,
		CommentA:   commentA,
		CommentBID: commentBID,
		CommentB:   commentB,
	}
}

// countQueuedOrDispatched returns the number of queued|dispatched tasks for
// (agent, issue). Mirrors the predicate used by HasPendingTaskForIssueAndAgent.
func countQueuedOrDispatched(t *testing.T, agentID, issueID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')
	`, issueID, agentID).Scan(&n); err != nil {
		t.Fatalf("count queued/dispatched tasks: %v", err)
	}
	return n
}

func TestEnqueueMentionedAgentTasks_SelfMentionCrossIssueSuppressed(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newSelfMentionFixture(t)

	if got := countQueuedOrDispatched(t, fx.JID, fx.IssueBID); got != 0 {
		t.Fatalf("before: expected 0 pending tasks on parent issue, got %d", got)
	}

	enqueueMentionedAgentTasksForTest(t, ctx, fx.IssueB, fx.CommentB, nil, "agent", fx.JID)

	if got := countQueuedOrDispatched(t, fx.JID, fx.IssueBID); got != 0 {
		t.Fatalf("after cross-issue self-mention: expected no queued task, got %d", got)
	}
}

func TestEnqueueMentionedAgentTasks_SelfMentionWhileRunningSuppressed(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newSelfMentionFixture(t)

	// Seed a running task for J on issue A — this is the agent's current run.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status)
		VALUES ($1, $2, $3, 'running')
	`, fx.JID, fx.RuntimeID, fx.IssueAID); err != nil {
		t.Fatalf("seed running task: %v", err)
	}

	if got := countQueuedOrDispatched(t, fx.JID, fx.IssueAID); got != 0 {
		t.Fatalf("before: expected 0 queued/dispatched tasks (only the running task), got %d", got)
	}

	enqueueMentionedAgentTasksForTest(t, ctx, fx.IssueA, fx.CommentA, nil, "agent", fx.JID)

	if got := countQueuedOrDispatched(t, fx.JID, fx.IssueAID); got != 0 {
		t.Fatalf("after self-mention while running: expected no follow-up, got %d", got)
	}
}

func TestComputeCommentAgentTriggers_SelfMentionOutcome(test *testing.T) {
	ctx := context.Background()
	fixture := newSelfMentionFixture(test)
	triggers, targets := testHandler.computeCommentAgentTriggers(ctx, fixture.IssueA, fixture.CommentA.Content, nil, "agent", fixture.JID, commentTriggerComputeOptions{})
	if len(triggers) != 0 || len(targets) != 1 {
		test.Fatalf("triggers=%d targets=%d, want 0 and 1", len(triggers), len(targets))
	}
	if targets[0].Status != DispatchBlocked || targets[0].ReasonCode != ReasonSelfTriggerSuppressed {
		test.Fatalf("target=%+v, want blocked/self_trigger_suppressed", targets[0])
	}
	triggers, _ = testHandler.computeCommentAgentTriggers(ctx, fixture.IssueA, fixture.CommentA.Content, nil, "member", testUserID, commentTriggerComputeOptions{})
	if len(triggers) != 1 {
		test.Fatalf("member mention triggers=%d, want 1", len(triggers))
	}
}

func TestCommentSelfMentionByMemberEditorSuppressed(test *testing.T) {
	fixture := newSelfMentionFixture(test)
	preview := previewCommentTriggersForTest(test, fixture.IssueAID, map[string]any{
		"content":            fixture.CommentA.Content,
		"editing_comment_id": fixture.CommentAID,
	})
	if len(preview.Agents) != 0 || len(preview.Blocked) != 1 || preview.Blocked[0].ReasonCode != ReasonSelfTriggerSuppressed {
		test.Fatalf("editing preview=%+v, want author suppressed", preview)
	}
	updateCommentForTriggerPreviewTest(test, fixture.CommentAID, map[string]any{"content": fixture.CommentA.Content + " updated"})
	if got := countQueuedOrDispatched(test, fixture.JID, fixture.IssueAID); got != 0 {
		test.Fatalf("member edit queued %d tasks for the comment author", got)
	}
}

func TestCommentSelfMentionSquadAndOtherAgent(test *testing.T) {
	ctx := context.Background()
	fixture := newSelfMentionFixture(test)
	squadID := dbfx.Squad(test, "self-mention squad", fixture.JID)
	otherID := createHandlerTestAgent(test, "other comment recipient", nil)
	for _, status := range []string{"running", "completed"} {
		test.Run(status, func(test *testing.T) {
			dbfx.Task(test, fixture.JID, testutil.Cols{
				"runtime_id": fixture.RuntimeID, "issue_id": fixture.IssueAID,
				"status": status, "squad_id": squadID, "is_leader_task": false,
			})
			content := "[self](mention://agent/" + fixture.JID + ") [squad](mention://squad/" + squadID + ") [other](mention://agent/" + otherID + ")"
			triggers, targets := testHandler.computeCommentAgentTriggers(ctx, fixture.IssueA, content, nil, "agent", fixture.JID, commentTriggerComputeOptions{})
			if len(triggers) != 1 || uuidToString(triggers[0].Agent.ID) != otherID {
				test.Fatalf("triggers=%+v, want other agent only", triggers)
			}
			outcomes := commentBlockedTargetOutcomes(targets)
			if len(outcomes) != 2 {
				test.Fatalf("outcomes=%+v, want both self mention forms blocked", outcomes)
			}
			for _, outcome := range outcomes {
				if outcome.ReasonCode != ReasonSelfTriggerSuppressed {
					test.Fatalf("outcome=%+v, want self_trigger_suppressed", outcome)
				}
			}
		})
	}
}

// TestEnqueueMentionedAgentTasks_SelfMentionDedupesAgainstPendingTask locks in
// that removing the self-trigger `continue` did NOT remove the standard
// HasPendingTaskForIssueAndAgent dedupe. If a queued or dispatched task
// already exists for the same agent on the same issue, a fresh self-mention
// must NOT pile on another duplicate.
func TestEnqueueMentionedAgentTasks_SelfMentionDedupesAgainstPendingTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newSelfMentionFixture(t)

	cases := []struct {
		name   string
		status string
	}{
		{name: "queued task blocks duplicate", status: "queued"},
		{name: "dispatched task blocks duplicate", status: "dispatched"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, fx.IssueAID); err != nil {
				t.Fatalf("reset tasks: %v", err)
			}
			if _, err := testPool.Exec(ctx, `
				INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, trigger_comment_id)
				VALUES ($1, $2, $3, $4, $5)
			`, fx.JID, fx.RuntimeID, fx.IssueAID, tc.status, fx.CommentA.ID); err != nil {
				t.Fatalf("seed %s task: %v", tc.status, err)
			}

			before := countQueuedOrDispatched(t, fx.JID, fx.IssueAID)
			if before != 1 {
				t.Fatalf("before: expected 1 pre-existing %s task, got %d", tc.status, before)
			}

			enqueueMentionedAgentTasksForTest(t, ctx, fx.IssueA, fx.CommentA, nil, "agent", fx.JID)

			after := countQueuedOrDispatched(t, fx.JID, fx.IssueAID)
			if after != 1 {
				t.Fatalf("after self-mention with pre-existing %s task: expected dedupe (still 1), got %d", tc.status, after)
			}
		})
	}
}
