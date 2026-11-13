package handler

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Follow provenance, scoped unfollow and the fork rule (CHE-3473 T3c, M12 and M3).

// followState is the active/unfollowed state of one issue's follow in lineage,
// or "" when there is no row.
func followState(t *testing.T, issue, lineage string) string {
	t.Helper()
	var state string
	rows, err := testPool.Query(t.Context(), `SELECT state FROM dm_progress_follow WHERE issue_id = $1 AND lineage_id = $2`, issue, lineage)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&state); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func rootScopeUnfollow(issue, session string) *http.Request {
	path := "/api/issues/" + issue + "/progress-follow?scope=root&chat_session_id=" + session
	return withURLParam(newRequest("DELETE", path, nil), "id", issue)
}

func childOf(t *testing.T, parent, title string) string {
	t.Helper()
	return createDMIssue(t, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": title, "parent_issue_id": parent}))
}

func TestDMProgress_RootUnsubscribeKeepsAnIndependentManualFollow(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	root := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	inheritedOnly := childOf(t, root, "inherited only")
	alsoManual := childOf(t, root, "inherited and manual")
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", alsoManual, "", dm)).Want(http.StatusOK)
	grandchild := childOf(t, inheritedOnly, "grandchild")

	// Scope "issue": only the root goes.
	testutil.Call(t, testHandler.UnfollowIssueProgress, progressReq("DELETE", root, "", dm)).Want(http.StatusOK)
	for name, issue := range map[string]string{"inherited child": inheritedOnly, "manual child": alsoManual, "grandchild": grandchild} {
		if got := followState(t, issue, dm); got != "active" {
			t.Errorf("issue-scope unfollow of the root left the %s %q, want active", name, got)
		}
	}

	// Scope "root": the root's whole subtree loses what it inherited from the root.
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", root, "", dm)).Want(http.StatusOK)
	testutil.Call(t, testHandler.UnfollowIssueProgress, rootScopeUnfollow(root, dm)).Want(http.StatusOK)
	if got := followState(t, root, dm); got != "unfollowed" {
		t.Errorf("root = %q, want unfollowed", got)
	}
	if got := followState(t, inheritedOnly, dm); got != "unfollowed" {
		t.Errorf("child that only inherited from the root = %q, want unfollowed", got)
	}
	if got := followState(t, grandchild, dm); got != "unfollowed" {
		t.Errorf("grandchild that only inherited from the root = %q, want unfollowed", got)
	}
	if got := followState(t, alsoManual, dm); got != "active" {
		t.Errorf("child with an independent manual follow = %q, want active", got)
	}
	if rows := followRows(t, alsoManual); len(rows) != 1 || !slices.Equal(rows[0].Reasons, []string{"manual"}) {
		t.Errorf("manual child = %+v, want only the manual reason left", rows)
	}

	// Only a followed root can be the target of a root-scope unfollow.
	testutil.Call(t, testHandler.UnfollowIssueProgress, rootScopeUnfollow(grandchild, dm)).Want(http.StatusNotFound)
	other := childOf(t, alsoManual, "child of the manual child")
	testutil.Call(t, testHandler.UnfollowIssueProgress, rootScopeUnfollow(other, dm)).Want(http.StatusBadRequest)
	if got := followState(t, other, dm); got != "active" {
		t.Errorf("refused root-scope unfollow changed the follow to %q", got)
	}
}

func TestDMProgress_UnfollowedIssueIsNotRecreatedByAChildEvent(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	root := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	child := childOf(t, root, "unfollowed child")
	testutil.Call(t, testHandler.UnfollowIssueProgress, progressReq("DELETE", child, "", dm)).Want(http.StatusOK)

	// A later child event asks again for the parent's follows to reach the child.
	tx, err := testHandler.TxStarter.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	q := testHandler.Queries.WithTx(tx)
	rootIssue, err := q.GetIssue(t.Context(), parseUUID(root))
	if err != nil {
		t.Fatal(err)
	}
	childIssue, err := q.GetIssue(t.Context(), parseUUID(child))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.InheritProgressFollows(t.Context(), q, rootIssue, childIssue); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := followState(t, child, dm); got != "unfollowed" {
		t.Fatalf("child follow after a child event = %q, want it to stay unfollowed", got)
	}

	// An explicit follow still brings it back.
	testutil.Call(t, testHandler.FollowIssueProgress, progressReq("POST", child, "", dm)).Want(http.StatusOK)
	if got := followState(t, child, dm); got != "active" {
		t.Fatalf("child follow after an explicit refollow = %q, want active", got)
	}
}

func TestDMProgress_ForkedDMHasNoFollows(t *testing.T) {
	w := newDMWorld(t)
	source := w.dm(t, testUserID)
	issue := w.agentIssue(t, w.task(t, source, testUserID), nil)
	forkMessage(t, source, testutil.Cols{"role": "user", "content": "goal"})

	var result ChatForkResponse
	testutil.Call(t, testHandler.ForkChatSession, forkChatRequest(t, source, "fork-follows")).Want(http.StatusCreated).JSON(&result)
	cleanupChatFork(t, result.Session.ID)
	fork := result.Session.ID

	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_follow WHERE lineage_id = $1`, fork); n != 0 {
		t.Errorf("fork inherited %d follows, want none", n)
	}
	var list struct {
		Follows []struct{} `json:"follows"`
	}
	testutil.Call(t, testHandler.ListDMProgressFollows, newRequest("GET", "/api/dm-progress/follows?chat_session_id="+fork, nil)).Want(http.StatusOK).JSON(&list)
	if len(list.Follows) != 0 {
		t.Errorf("fork lists %d follows, want none", len(list.Follows))
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id = $1 AND continued_into_session_id IS NOT NULL`, source); n != 0 {
		t.Error("fork set continued_into_session_id on the source")
	}

	// New lineage: the fork is its own root, and the source keeps its follow.
	q := testHandler.Queries
	root, err := q.ResolveChatLineageRoot(t.Context(), parseUUID(fork))
	if err != nil || uuidToString(root) != fork {
		t.Errorf("fork lineage root = %s (%v), want the fork itself", uuidToString(root), err)
	}
	if got := followState(t, issue, source); got != "active" {
		t.Errorf("source follow after the fork = %q, want active", got)
	}
	forkIssue := w.agentIssue(t, w.task(t, fork, testUserID), map[string]any{"title": "created in the fork"})
	if got := followState(t, forkIssue, fork); got != "active" || followState(t, forkIssue, source) != "" {
		t.Errorf("issue created in the fork follows in the fork lane %q, source lane %q", got, followState(t, forkIssue, source))
	}
}
