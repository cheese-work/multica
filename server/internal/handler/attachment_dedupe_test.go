package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9490: a retried `comment add --attachment` uploads the same file again.
// An identical re-upload (same issue, same name, same bytes) must hand back the
// attachment already there instead of adding a copy.
func TestAttachmentIdenticalReuploadReturnsExisting(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "dedupe identical upload", nil)
	_, first := uploadRev(t, issueID, "plan-r1.md", "same bytes\n", "")
	resp, second := uploadRev(t, issueID, "plan-r1.md", "same bytes\n", "")
	resp.Want(http.StatusOK)
	if second.ID != first.ID || second.Revision != 1 {
		t.Fatalf("re-upload = %+v, want the existing attachment %s at revision 1", second, first.ID)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM attachment WHERE issue_id = $1`, issueID); n != 1 {
		t.Fatalf("attachment rows = %d, want 1", n)
	}
	// Changed bytes still chain as the next revision.
	_, third := uploadRev(t, issueID, "plan-r1.md", "other bytes\n", "")
	if third.Revision != 2 || third.RevisionOf == nil || *third.RevisionOf != first.ID {
		t.Fatalf("changed upload = %+v, want revision 2 of %s", third, first.ID)
	}
}

// A file already linked to a comment is not the target: the next comment needs
// its own unlinked copy to attach.
func TestAttachmentReuploadOfCommentLinkedFileCreatesNew(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "dedupe skips linked", nil)
	_, first := uploadRev(t, issueID, "plan.md", "x\n", "")
	createCommentWithAttachment(t, testHandler, issueID, first.ID).Want(http.StatusCreated)
	_, second := uploadRev(t, issueID, "plan.md", "x\n", "")
	if second.ID == first.ID {
		t.Fatalf("re-upload returned the comment-linked attachment %s", first.ID)
	}
}

// An explicit --replaces always makes a revision.
func TestAttachmentReplacesIsNotDeduped(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "dedupe skips replaces", nil)
	_, first := uploadRev(t, issueID, "plan.md", "x\n", "")
	_, second := uploadRev(t, issueID, "plan.md", "x\n", first.ID)
	if second.ID == first.ID || second.Revision != 2 {
		t.Fatalf("replaces upload = %+v, want a new revision 2", second)
	}
}

func agentActorRequest(t *testing.T, method, path string, agentID, taskID string) *http.Request {
	t.Helper()
	req := newRequest(method, path, nil)
	return testutil.WithHeaders(req, "X-Agent-ID", agentID, "X-Task-ID", taskID)
}

func deleteAttachmentAs(t *testing.T, id, agentID, taskID string) *testutil.Response {
	t.Helper()
	req := withURLParam(agentActorRequest(t, http.MethodDelete, "/api/attachments/"+id, agentID, taskID), "id", id)
	return testutil.Call(t, testHandler.DeleteAttachment, req)
}

func agentOrphan(t *testing.T, issueID, agentID string) string {
	t.Helper()
	return dbfx.Insert(t, "attachment", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": issueID,
		"uploader_type": "agent", "uploader_id": agentID,
		"filename": "plan.md", "url": "https://example.test/plan.md",
		"content_type": "text/markdown", "size_bytes": 1,
	})
}

func agentRunningTask(t *testing.T, agentID, issueID string) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": handlerTestRuntimeID(t),
		"status":     "running",
		"issue_id":   issueID,
		"started_at": testutil.Raw("now()"),
	})
}

// The agent that uploaded a file can delete it while no comment references it.
func TestDeleteAttachmentByUploaderAgentWhenOrphan(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "agent deletes orphan", nil)
	agentID := createHandlerTestAgent(t, "Attachment Orphan Uploader", nil)
	taskID := agentRunningTask(t, agentID, issueID)
	id := agentOrphan(t, issueID, agentID)

	deleteAttachmentAs(t, id, agentID, taskID).Want(http.StatusNoContent)
	if n := dbfx.Count(t, `SELECT count(*) FROM attachment WHERE id = $1`, id); n != 0 {
		t.Fatalf("attachment rows = %d, want 0", n)
	}
}

// A comment-linked file stays: reviewers pin its SHA-256.
func TestDeleteAttachmentByAgentRefusesCommentLinked(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "agent keeps linked", nil)
	agentID := createHandlerTestAgent(t, "Attachment Linked Uploader", nil)
	taskID := agentRunningTask(t, agentID, issueID)
	id := agentOrphan(t, issueID, agentID)
	createCommentWithAttachment(t, testHandler, issueID, id).Want(http.StatusCreated)

	deleteAttachmentAs(t, id, agentID, taskID).Want(http.StatusConflict)
	if n := dbfx.Count(t, `SELECT count(*) FROM attachment WHERE id = $1`, id); n != 1 {
		t.Fatalf("attachment rows = %d, want 1", n)
	}
}

// Another agent's orphan is not yours to delete.
func TestDeleteAttachmentByOtherAgentRefused(t *testing.T) {
	withMockStorage(t)
	issueID := dbfx.Issue(t, "other agent cannot delete", nil)
	owner := createHandlerTestAgent(t, "Attachment Owner", nil)
	other := createHandlerTestAgent(t, "Attachment Other", nil)
	taskID := agentRunningTask(t, other, issueID)
	id := agentOrphan(t, issueID, owner)

	deleteAttachmentAs(t, id, other, taskID).Want(http.StatusForbidden)
	if n := dbfx.Count(t, `SELECT count(*) FROM attachment WHERE id = $1`, id); n != 1 {
		t.Fatalf("attachment rows = %d, want 1", n)
	}
	_ = context.Background()
}

// A marked task's upload keeps its external-read mark: it never reuses an
// unmarked copy, and a clean upload never reuses a marked one.
func TestAttachmentDedupeKeepsExternalReadMark(t *testing.T) {
	chain := newCrossIssueChain(t, testUserID, testUserID)
	marked := markedTask(t, chain.CoordinatorID, true)
	clean := markedTask(t, chain.CoordinatorID, false)

	cleanID := uploadToIssue(t, chain.CoordinatorID, clean, chain.IssueX)
	markedID := uploadToIssue(t, chain.CoordinatorID, marked, chain.IssueX)
	if markedID == cleanID {
		t.Fatalf("marked upload reused the unmarked attachment %s", cleanID)
	}
	if again := uploadToIssue(t, chain.CoordinatorID, marked, chain.IssueX); again != markedID {
		t.Fatalf("identical marked re-upload = %s, want %s", again, markedID)
	}
}
