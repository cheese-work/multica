package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func previewCommentForTest(commentID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := newRequest("GET", "/api/comments/"+commentID+"/preview", nil)
	r = withURLParam(r, "commentId", commentID)
	testHandler.GetCommentPreview(w, r)
	return w
}

func TestGetCommentPreview(t *testing.T) {
	fx := newResolveTestFixture(t)
	body := "first line of the comment\nsecond line stays out"
	var commentID string
	if err := testPool.QueryRow(t.Context(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, $4, 'comment') RETURNING id
	`, fx.IssueID, testWorkspaceID, testUserID, body).Scan(&commentID); err != nil {
		t.Fatalf("insert comment: %v", err)
	}

	t.Run("returns issue key, author and first line", func(t *testing.T) {
		w := previewCommentForTest(commentID)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var resp CommentPreviewResponse
		decodeJSON(t, w.Body.String(), &resp)
		if resp.CommentID != commentID || resp.IssueID != fx.IssueID {
			t.Fatalf("ids = %+v, want comment %s issue %s", resp, commentID, fx.IssueID)
		}
		if resp.IssueIdentifier == "" {
			t.Fatalf("issue_identifier empty: %+v", resp)
		}
		if resp.AuthorName != handlerTestName {
			t.Fatalf("author_name = %q, want %q", resp.AuthorName, handlerTestName)
		}
		if resp.Preview != "first line of the comment" {
			t.Fatalf("preview = %q", resp.Preview)
		}
	})

	t.Run("unknown comment is 404", func(t *testing.T) {
		w := previewCommentForTest("00000000-0000-4000-8000-000000000000")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", w.Code)
		}
	})

	t.Run("comment in another workspace is 404 with no preview", func(t *testing.T) {
		var otherWS, otherIssue, otherComment string
		if err := testPool.QueryRow(t.Context(), `
			INSERT INTO workspace (name, slug, description, issue_prefix)
			VALUES ('Other', 'comment-preview-other', '', 'OTH') RETURNING id
		`).Scan(&otherWS); err != nil {
			t.Fatalf("insert workspace: %v", err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM comment WHERE workspace_id = $1`, otherWS)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE workspace_id = $1`, otherWS)
			testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, otherWS)
		})
		if err := testPool.QueryRow(t.Context(), `
			INSERT INTO issue (workspace_id, creator_type, creator_id, title)
			VALUES ($1, 'member', $2, 'secret') RETURNING id
		`, otherWS, testUserID).Scan(&otherIssue); err != nil {
			t.Fatalf("insert issue: %v", err)
		}
		if err := testPool.QueryRow(t.Context(), `
			INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
			VALUES ($1, $2, 'member', $3, 'secret body', 'comment') RETURNING id
		`, otherIssue, otherWS, testUserID).Scan(&otherComment); err != nil {
			t.Fatalf("insert comment: %v", err)
		}
		w := previewCommentForTest(otherComment)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", w.Code)
		}
		if got := w.Body.String(); strings.Contains(got, "secret") {
			t.Fatalf("response leaked content: %s", got)
		}
	})
}
