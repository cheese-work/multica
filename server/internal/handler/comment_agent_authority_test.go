package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// An agent run carries its owner's workspace role on the task token, so the
// owner/admin override must not apply to it: an agent edits or deletes only
// comments its own agent wrote (CHE-10266). Humans keep the override.

type commentAuthorityFixture struct {
	issueID                string
	agentA, taskA          string
	agentB, taskB          string
	verdictByB, replyByB   string
	tombstoneParent        string
	ownCommentA, memberTxt string
}

func newCommentAuthorityFixture(t *testing.T) commentAuthorityFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	f := commentAuthorityFixture{issueID: dbfx.Issue(t, "agent comment authority")}
	f.agentA = createHandlerTestAgent(t, "authority-agent-a", nil)
	f.taskA = createHandlerTestTaskForAgent(t, f.agentA)
	f.agentB = createHandlerTestAgent(t, "authority-agent-b", nil)
	f.taskB = createHandlerTestTaskForAgent(t, f.agentB)
	f.verdictByB = dbfx.Comment(t, f.issueID, "Verdict FAIL\nReviewer: agent-b", testutil.Cols{"author_type": "agent", "author_id": f.agentB})
	f.ownCommentA = dbfx.Comment(t, f.issueID, "agent a note", testutil.Cols{"author_type": "agent", "author_id": f.agentA})
	// A tombstoned root by B whose only reply is B's: deleting the reply
	// would also prune the tombstone.
	f.tombstoneParent = dbfx.Comment(t, f.issueID, "", testutil.Cols{"author_type": "agent", "author_id": f.agentB})
	dbfx.Exec(t, `UPDATE comment SET deleted_at = now() WHERE id = $1`, f.tombstoneParent)
	f.replyByB = dbfx.Comment(t, f.issueID, "reply by b", testutil.Cols{"author_type": "agent", "author_id": f.agentB, "parent_id": f.tombstoneParent})
	return f
}

// asAgent stamps the request the way the task-token auth middleware does.
func asAgent(req *http.Request, agentID, taskID string) *http.Request {
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	return req
}

func updateComment(t *testing.T, req *http.Request, id string) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateComment, withURLParam(req, "commentId", id))
}

func deleteComment(t *testing.T, req *http.Request, id string) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.DeleteComment, withURLParam(req, "commentId", id))
}

func TestAgentCannotEditAnotherAgentsComment(t *testing.T) {
	f := newCommentAuthorityFixture(t)
	req := asAgent(newRequest(http.MethodPut, "/api/comments/"+f.verdictByB, map[string]any{"content": "Verdict PASS"}), f.agentA, f.taskA)
	updateComment(t, req, f.verdictByB).Want(http.StatusForbidden)
	if got := loadCommentRowState(t, f.verdictByB); got.content != "Verdict FAIL\nReviewer: agent-b" {
		t.Fatalf("verdict content = %q, want it unchanged", got.content)
	}
}

func TestAgentCannotDeleteAnotherAgentsComment(t *testing.T) {
	f := newCommentAuthorityFixture(t)
	req := asAgent(newRequest(http.MethodDelete, "/api/comments/"+f.verdictByB, nil), f.agentA, f.taskA)
	deleteComment(t, req, f.verdictByB).Want(http.StatusForbidden)
	if got := loadCommentRowState(t, f.verdictByB); !got.exists || got.deleted {
		t.Fatalf("verdict = %+v, want it untouched", got)
	}
}

func TestAgentCannotPruneAnotherAgentsTombstoneThroughReplyDelete(t *testing.T) {
	f := newCommentAuthorityFixture(t)
	req := asAgent(newRequest(http.MethodDelete, "/api/comments/"+f.replyByB, nil), f.agentA, f.taskA)
	deleteComment(t, req, f.replyByB).Want(http.StatusForbidden)
	if got := loadCommentRowState(t, f.replyByB); !got.exists {
		t.Fatal("reply was deleted")
	}
	if got := loadCommentRowState(t, f.tombstoneParent); !got.exists {
		t.Fatal("tombstone was pruned")
	}
}

func TestAgentCannotModifyAHumansComment(t *testing.T) {
	f := newCommentAuthorityFixture(t)
	human := dbfx.Comment(t, f.issueID, "human note")
	req := asAgent(newRequest(http.MethodPut, "/api/comments/"+human, map[string]any{"content": "x"}), f.agentA, f.taskA)
	updateComment(t, req, human).Want(http.StatusForbidden)
	req = asAgent(newRequest(http.MethodDelete, "/api/comments/"+human, nil), f.agentA, f.taskA)
	deleteComment(t, req, human).Want(http.StatusForbidden)
}

func TestAgentCanEditAndDeleteItsOwnComment(t *testing.T) {
	f := newCommentAuthorityFixture(t)
	req := asAgent(newRequest(http.MethodPut, "/api/comments/"+f.ownCommentA, map[string]any{"content": "agent a edited"}), f.agentA, f.taskA)
	updateComment(t, req, f.ownCommentA).Want(http.StatusOK)
	if got := loadCommentRowState(t, f.ownCommentA); got.content != "agent a edited" {
		t.Fatalf("content = %q, want the edit", got.content)
	}
	req = asAgent(newRequest(http.MethodDelete, "/api/comments/"+f.ownCommentA, nil), f.agentA, f.taskA)
	deleteComment(t, req, f.ownCommentA).Want(http.StatusNoContent)
	if got := loadCommentRowState(t, f.ownCommentA); got.exists {
		t.Fatal("own comment still exists")
	}
}

func TestHumanOwnerCanEditAndDeleteAnAgentsComment(t *testing.T) {
	f := newCommentAuthorityFixture(t)
	updateComment(t, newRequest(http.MethodPut, "/api/comments/"+f.verdictByB, map[string]any{"content": "edited by owner"}), f.verdictByB).Want(http.StatusOK)
	deleteComment(t, newRequest(http.MethodDelete, "/api/comments/"+f.verdictByB, nil), f.verdictByB).Want(http.StatusNoContent)
}

func TestHumanAdminCanEditAndDeleteAnAgentsComment(t *testing.T) {
	f := newCommentAuthorityFixture(t)
	adminID := createPermissionTestAdmin(t, "che10266-admin@multica.test")
	updateComment(t, newRequestAs(adminID, http.MethodPut, "/api/comments/"+f.ownCommentA, map[string]any{"content": "edited by admin"}), f.ownCommentA).Want(http.StatusOK)
	deleteComment(t, newRequestAs(adminID, http.MethodDelete, "/api/comments/"+f.ownCommentA, nil), f.ownCommentA).Want(http.StatusNoContent)
}

func TestCanModifyComment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		actorType string
		isAuthor  bool
		role      string
		want      bool
	}{
		{"agent author", "agent", true, "owner", true},
		{"agent owner role, not author", "agent", false, "owner", false},
		{"agent admin role, not author", "agent", false, "admin", false},
		{"agent member role, not author", "agent", false, "member", false},
		{"member author", "member", true, "member", true},
		{"member owner, not author", "member", false, "owner", true},
		{"member admin, not author", "member", false, "admin", true},
		{"member plain, not author", "member", false, "member", false},
	} {
		if got := CanModifyComment(tc.actorType, tc.isAuthor, tc.role); got != tc.want {
			t.Errorf("%s: CanModifyComment = %v, want %v", tc.name, got, tc.want)
		}
	}
}
