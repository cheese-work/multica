package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// External-read mark (CHE-2216 plan 3.7, slice B8a; T16 mark). A marked task's
// comments, description edits and attachments store the mark; everything else
// stays unmarked. The writes go through the real handlers.

func markedTask(t *testing.T, agentID string, marked bool) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(t), "started_at": testutil.Raw("now()"), "external_read": marked})
}

func flag(t *testing.T, query string, args ...any) bool {
	t.Helper()
	var v bool
	dbfx.QueryRow(t, query, args...).Scan(&v)
	return v
}

func TestMarkedTaskCommentStoresMark(t *testing.T) {
	chain := newCrossIssueChain(t, testUserID, testUserID)
	marked := markedTask(t, chain.CoordinatorID, true)
	clean := markedTask(t, chain.CoordinatorID, false)

	var byMarked, byClean CommentResponse
	agentComments(t, chain.CoordinatorID, marked, chain.IssueX, "from a marked task", "").Want(http.StatusCreated).JSON(&byMarked)
	agentComments(t, chain.CoordinatorID, clean, chain.IssueX, "from a clean task", "").Want(http.StatusCreated).JSON(&byClean)
	byMember := dbfx.Comment(t, chain.IssueX, "member")

	const q = `SELECT external_read FROM comment WHERE id = $1`
	if !flag(t, q, byMarked.ID) {
		t.Errorf("comment by marked task: external_read = false")
	}
	if flag(t, q, byClean.ID) || flag(t, q, byMember) {
		t.Errorf("comment by clean task or member is marked")
	}
	// The mark only grows: an update cannot clear it.
	dbfx.Exec(t, `UPDATE comment SET external_read = false, content = 'edited' WHERE id = $1`, byMarked.ID)
	if !flag(t, q, byMarked.ID) {
		t.Errorf("update cleared the comment mark")
	}
}

func TestMarkedTaskDescriptionEditStoresMark(t *testing.T) {
	chain := newCrossIssueChain(t, testUserID, testUserID)
	marked := markedTask(t, chain.CoordinatorID, true)
	clean := markedTask(t, chain.CoordinatorID, false)
	edit := func(issueID, taskID string) {
		testutil.Call(t, testHandler.UpdateIssue, testutil.WithURLParams(
			asRun(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"description": "edited by " + taskID}), chain.CoordinatorID, taskID),
			"id", issueID,
		)).Want(http.StatusOK)
	}
	const q = `SELECT description_external_read FROM issue WHERE id = $1`

	edit(chain.IssueY, clean)
	if flag(t, q, chain.IssueY) {
		t.Fatalf("description edit by clean task is marked")
	}
	edit(chain.IssueY, marked)
	if !flag(t, q, chain.IssueY) {
		t.Errorf("description edit by marked task: description_external_read = false")
	}
	// A later clean edit cannot clear it.
	edit(chain.IssueY, clean)
	if !flag(t, q, chain.IssueY) {
		t.Errorf("clean edit cleared the description mark")
	}
	if flag(t, q, chain.IssueX) {
		t.Errorf("untouched issue is marked")
	}
}

func TestMarkedTaskCreatedIssueDescriptionStoresMark(t *testing.T) {
	chain := newCrossIssueChain(t, testUserID, testUserID)
	marked := markedTask(t, chain.CoordinatorID, true)
	var created struct {
		ID string `json:"id"`
	}
	testutil.Call(t, testHandler.CreateIssue, asRun(
		newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
			"title": "B8a created by marked task", "description": "hostile body", "status": "backlog", "priority": "medium",
			"parent_issue_id": chain.IssueX,
		}), chain.CoordinatorID, marked)).Want(http.StatusCreated).JSON(&created)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, created.ID)
	if !flag(t, `SELECT description_external_read FROM issue WHERE id = $1`, created.ID) {
		t.Errorf("issue created with a description by a marked task: description_external_read = false")
	}
}

func uploadToIssue(t *testing.T, agentID, taskID, issueID string) string {
	t.Helper()
	orig := testHandler.Storage
	testHandler.Storage = &mockStorage{}
	t.Cleanup(func() { testHandler.Storage = orig })

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("plain text"))
	mw.WriteField("issue_id", issueID)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/upload-file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	w := httptest.NewRecorder()
	testHandler.UploadFile(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var resp AttachmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM attachment WHERE id = $1`, resp.ID)
	return resp.ID
}

func TestMarkedTaskAttachmentStoresMark(t *testing.T) {
	chain := newCrossIssueChain(t, testUserID, testUserID)
	marked := markedTask(t, chain.CoordinatorID, true)
	clean := markedTask(t, chain.CoordinatorID, false)

	const q = `SELECT external_read FROM attachment WHERE id = $1`
	if !flag(t, q, uploadToIssue(t, chain.CoordinatorID, marked, chain.IssueX)) {
		t.Errorf("attachment by marked task: external_read = false")
	}
	if flag(t, q, uploadToIssue(t, chain.CoordinatorID, clean, chain.IssueX)) {
		t.Errorf("attachment by clean task is marked")
	}
}

func TestDaemonMarksTaskExternalRead(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "External read runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "External read agent")
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID, "issue_id": issueID, "started_at": testutil.Raw("now()")})
	mark := func() {
		req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+taskID+"/external-read", nil), "taskId", taskID)
		testutil.Call(t, testHandler.MarkTaskExternalRead, req).Want(http.StatusOK)
	}
	const q = `SELECT external_read FROM agent_task_queue WHERE id = $1`
	if flag(t, q, taskID) {
		t.Fatalf("fresh task is marked")
	}
	mark()
	if !flag(t, q, taskID) {
		t.Fatalf("task not marked after the daemon report")
	}
	mark() // idempotent
	if !flag(t, q, taskID) {
		t.Fatalf("second report cleared the mark")
	}
}
