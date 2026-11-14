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
