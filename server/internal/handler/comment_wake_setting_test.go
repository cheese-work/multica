package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5907 (C3): comment_wake = mentioned_only turns off the implicit routing of
// an unmentioned member comment. Explicit mentions are unchanged.

func setCommentWakeForTest(t *testing.T, mode string) {
	t.Helper()
	setAgentCommentWakePolicyForTest(t, false) // restores workspace settings on cleanup
	saveWorkspaceSettings(t, testWorkspaceID, map[string]any{
		agentCommentWakePolicySetting: false,
		"comment_wake":                mode,
	})
}

func memberTriggers(t *testing.T, issue db.Issue, content string, parent *db.Comment) []commentAgentTrigger {
	t.Helper()
	triggers, _ := testHandler.computeCommentAgentTriggers(context.Background(), issue, content, parent, "member", testUserID, commentTriggerComputeOptions{OriginatorUserID: testUserID})
	return triggers
}

func agentRootedThread(t *testing.T, fx squadCommentTriggerFixture) db.Comment {
	t.Helper()
	issueID := uuidToString(fx.Issue.ID)
	rootID := dbfx.Comment(t, issueID, "root by agent A", testutil.Cols{"author_type": "agent", "author_id": fx.LeaderID})
	dbfx.Comment(t, issueID, "later by agent B", testutil.Cols{"author_type": "agent", "author_id": fx.OtherID, "parent_id": rootID})
	root, err := testHandler.Queries.GetComment(context.Background(), parseUUID(rootID))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCommentWakeUnmentionedReplyInAgentThread(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	root := agentRootedThread(t, fx)

	setCommentWakeForTest(t, "conversation")
	if got := memberTriggers(t, fx.Issue, "ok", &root); len(got) != 1 {
		t.Fatalf("conversation: triggers = %d, want 1 (today's behaviour)", len(got))
	}
	setCommentWakeForTest(t, "mentioned_only")
	if got := memberTriggers(t, fx.Issue, "ok", &root); len(got) != 0 {
		t.Fatalf("mentioned_only: triggers = %+v, want none", got)
	}
}

func TestCommentWakeRootCommentNoAssigneeOrLeaderRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t) // issue assigned to a squad

	setCommentWakeForTest(t, "conversation")
	if got := memberTriggers(t, fx.Issue, "status?", nil); len(got) != 1 {
		t.Fatalf("conversation: triggers = %d, want 1 (leader)", len(got))
	}
	setCommentWakeForTest(t, "mentioned_only")
	if got := memberTriggers(t, fx.Issue, "status?", nil); len(got) != 0 {
		t.Fatalf("mentioned_only: triggers = %+v, want none", got)
	}
}

func TestCommentWakeBlockedIssueNoAssigneeAdd(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	root := agentRootedThread(t, fx)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='blocked' WHERE id=$1`, fx.Issue.ID); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), fx.Issue.ID)
	if err != nil {
		t.Fatal(err)
	}

	setCommentWakeForTest(t, "conversation")
	if got := memberTriggers(t, issue, "ok", &root); len(got) != 2 {
		t.Fatalf("conversation: triggers = %d, want 2 (last agent + assignee)", len(got))
	}
	setCommentWakeForTest(t, "mentioned_only")
	if got := memberTriggers(t, issue, "ok", &root); len(got) != 0 {
		t.Fatalf("mentioned_only: triggers = %+v, want none", got)
	}
}

func TestCommentWakeExplicitMentionRunsOnlyThatAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	root := agentRootedThread(t, fx)

	setCommentWakeForTest(t, "mentioned_only")
	got := memberTriggers(t, fx.Issue, "[@A](mention://agent/"+fx.LeaderID+") ok", &root)
	if len(got) != 1 || uuidToString(got[0].Agent.ID) != fx.LeaderID {
		t.Fatalf("triggers = %+v, want exactly the mentioned agent", got)
	}
}

func TestCommentWakeIssueScopeOverridesWorkspace(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	root := agentRootedThread(t, fx)
	setCommentWakeForTest(t, "conversation")
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET metadata='{"comment_wake":"mentioned_only"}' WHERE id=$1`, fx.Issue.ID); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), fx.Issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := memberTriggers(t, issue, "ok", &root); len(got) != 0 {
		t.Fatalf("issue mentioned_only: triggers = %+v, want none", got)
	}
}

func TestCommentWakeSettingRejectsUnknownMode(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true) // restores workspace settings on cleanup
	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"settings": map[string]any{"comment_wake": "sometimes"}}), "id", testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusBadRequest)
}

// Project scope (CHE-5907 round 1, P1): issue metadata > project settings >
// workspace settings.

// projectIssueWithWake puts the fixture issue in a new project whose settings
// hold projectMode ("" = no comment_wake key) and returns the reloaded issue.
func projectIssueWithWake(t *testing.T, fx squadCommentTriggerFixture, projectMode string) db.Issue {
	t.Helper()
	ctx := context.Background()
	projectID := dbfx.Project(t, "wake project")
	settings := `{}`
	if projectMode != "" {
		settings = `{"comment_wake":"` + projectMode + `"}`
	}
	if _, err := testPool.Exec(ctx, `UPDATE project SET settings=$2::jsonb WHERE id=$1`, projectID, settings); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE issue SET project_id=$2 WHERE id=$1`, fx.Issue.ID, projectID); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, fx.Issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func TestCommentWakeProjectScopeOverridesWorkspace(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	root := agentRootedThread(t, fx)

	setCommentWakeForTest(t, "conversation")
	issue := projectIssueWithWake(t, fx, "mentioned_only")
	if got := memberTriggers(t, issue, "ok", &root); len(got) != 0 {
		t.Fatalf("project mentioned_only over workspace conversation: triggers = %+v, want none", got)
	}

	setCommentWakeForTest(t, "mentioned_only")
	issue = projectIssueWithWake(t, fx, "conversation")
	if got := memberTriggers(t, issue, "ok", &root); len(got) != 1 {
		t.Fatalf("project conversation over workspace mentioned_only: triggers = %d, want 1", len(got))
	}
}

func TestCommentWakeProjectWithoutSettingFallsBackToWorkspace(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	root := agentRootedThread(t, fx)
	setCommentWakeForTest(t, "mentioned_only")
	issue := projectIssueWithWake(t, fx, "")
	if got := memberTriggers(t, issue, "ok", &root); len(got) != 0 {
		t.Fatalf("project without comment_wake: triggers = %+v, want workspace mentioned_only (none)", got)
	}
}

func TestCommentWakeIssueScopeOverridesProject(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	root := agentRootedThread(t, fx)
	setCommentWakeForTest(t, "mentioned_only")
	issue := projectIssueWithWake(t, fx, "mentioned_only")
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET metadata='{"comment_wake":"conversation"}' WHERE id=$1`, issue.ID); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := memberTriggers(t, issue, "ok", &root); len(got) != 1 {
		t.Fatalf("issue conversation over project mentioned_only: triggers = %d, want 1", len(got))
	}
}
