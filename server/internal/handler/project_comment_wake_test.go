package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5907: PUT /api/projects/{id}/comment-wake sets the project tier of comment_wake.

func putProjectCommentWake(t *testing.T, projectID, userID string, body map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(workflowRequest(http.MethodPut, "/api/projects/"+projectID+"/comment-wake", body), "id", projectID)
	req.Header.Set("X-User-ID", userID)
	return testutil.Call(t, testHandler.SetProjectCommentWake, req)
}

func storedProjectSettings(t *testing.T, projectID string) string {
	t.Helper()
	var s string
	if err := testPool.QueryRow(context.Background(), `SELECT settings::text FROM project WHERE id=$1`, projectID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProjectCommentWakeSetAndClear(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	f := newSplitFixture(t) // project led by f.lead; f.admin may manage it

	putProjectCommentWake(t, f.projectID, f.admin, map[string]any{"comment_wake": "mentioned_only"}).Want(http.StatusOK)
	if got := storedProjectSettings(t, f.projectID); got != `{"comment_wake": "mentioned_only"}` {
		t.Fatalf("settings = %s, want mentioned_only", got)
	}
	putProjectCommentWake(t, f.projectID, f.admin, map[string]any{"comment_wake": nil}).Want(http.StatusOK)
	if got := storedProjectSettings(t, f.projectID); got != `{}` {
		t.Fatalf("settings = %s, want {} after clear", got)
	}
}

func TestProjectCommentWakeRejectsUnknownModeAndPlainMember(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	f := newSplitFixture(t)
	putProjectCommentWake(t, f.projectID, f.admin, map[string]any{"comment_wake": "sometimes"}).Want(http.StatusBadRequest)
	putProjectCommentWake(t, f.projectID, f.member, map[string]any{"comment_wake": "mentioned_only"}).Want(http.StatusForbidden)
	if got := storedProjectSettings(t, f.projectID); got != `{}` {
		t.Fatalf("settings = %s, want untouched {}", got)
	}
}
