package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// supersedeFixture is an issue with agent-authored threads, the shape of a plan
// revision and its review verdicts.
type supersedeFixture struct {
	IssueID string
}

func newSupersedeFixture(t *testing.T) supersedeFixture {
	t.Helper()
	issueID := dbfx.Issue(t, "supersede fixture")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM comment_supersession WHERE issue_id = $1`, issueID)
	})
	return supersedeFixture{IssueID: issueID}
}

// insert adds a comment as author (a random agent id for "agent") created at
// offset from now.
func (fx supersedeFixture) insert(t *testing.T, authorType string, parent *string, offset time.Duration, body string) string {
	t.Helper()
	authorID := testUserID
	if authorType == "agent" {
		authorID = uuid.NewString()
	}
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, parent_id, created_at)
		VALUES ($1, $2, $3, $4, $5, 'comment', $6, $7)
		RETURNING id
	`, fx.IssueID, testWorkspaceID, authorType, authorID, body, parent, time.Now().Add(offset)).Scan(&id); err != nil {
		t.Fatalf("insert comment %q: %v", body, err)
	}
	return id
}

func (fx supersedeFixture) post(t *testing.T, body map[string]any) (*httptest.ResponseRecorder, CommentResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	r := newRequest(http.MethodPost, "/api/issues/"+fx.IssueID+"/comments", body)
	r = withURLParam(r, "id", fx.IssueID)
	testHandler.CreateComment(w, r)
	var resp CommentResponse
	if w.Code == http.StatusCreated {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
	}
	return w, resp
}

func supersededBy(t *testing.T, commentID string) (string, bool) {
	t.Helper()
	var by string
	err := testPool.QueryRow(context.Background(),
		`SELECT superseding_comment_id FROM comment_supersession WHERE superseded_comment_id = $1`, commentID).Scan(&by)
	return by, err == nil
}

func TestCreateComment_SupersedesResolvesAgentThread(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSupersedeFixture(t)
	plan1 := fx.insert(t, "agent", nil, -3*time.Hour, "plan rev-1")
	verdict1 := fx.insert(t, "agent", &plan1, -2*time.Hour, "verdict on rev-1")

	w, resp := fx.post(t, map[string]any{"content": "plan rev-2", "supersedes": []string{plan1}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}

	if !commentResolved(t, plan1) {
		t.Fatalf("superseded thread root must be resolved")
	}
	if commentResolved(t, verdict1) {
		t.Fatalf("root-resolved thread must not also resolve its reply")
	}
	if by, ok := supersededBy(t, plan1); !ok || by != resp.ID {
		t.Fatalf("link = %q (found %t), want %q", by, ok, resp.ID)
	}
	if len(resp.Supersedes) != 1 || resp.Supersedes[0].CommentID != plan1 || !resp.Supersedes[0].Resolved {
		t.Fatalf("response supersedes = %+v, want resolved plan1", resp.Supersedes)
	}
}

func TestCreateComment_SupersedesViaReplyIDResolvesThreadRoot(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSupersedeFixture(t)
	plan1 := fx.insert(t, "agent", nil, -3*time.Hour, "plan rev-1")
	verdict1 := fx.insert(t, "agent", &plan1, -2*time.Hour, "verdict on rev-1")

	if w, _ := fx.post(t, map[string]any{"content": "plan rev-2", "supersedes": []string{verdict1}}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if !commentResolved(t, plan1) {
		t.Fatalf("passing a reply id must resolve its thread root")
	}
}

func TestCreateComment_SupersedeSkipsThreadWhenHumanPostedLatestReply(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSupersedeFixture(t)
	plan1 := fx.insert(t, "agent", nil, -3*time.Hour, "plan rev-1")
	fx.insert(t, "member", &plan1, -2*time.Hour, "human feedback")

	w, resp := fx.post(t, map[string]any{"content": "plan rev-2", "supersedes": []string{plan1}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if commentResolved(t, plan1) {
		t.Fatalf("a thread whose latest reply is human must stay open")
	}
	if len(resp.Supersedes) != 1 || resp.Supersedes[0].Resolved || resp.Supersedes[0].SkippedReason != "human_reply" {
		t.Fatalf("response supersedes = %+v, want skipped human_reply", resp.Supersedes)
	}
}

func TestCreateComment_SupersedeSkipsThreadWithReplyNewerThanRequest(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSupersedeFixture(t)
	plan1 := fx.insert(t, "agent", nil, -3*time.Hour, "plan rev-1")
	fx.insert(t, "agent", &plan1, time.Hour, "reply newer than the request")

	w, resp := fx.post(t, map[string]any{"content": "plan rev-2", "supersedes": []string{plan1}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if commentResolved(t, plan1) {
		t.Fatalf("a thread with a reply newer than the request must stay open")
	}
	if len(resp.Supersedes) != 1 || resp.Supersedes[0].SkippedReason != "thread_changed" {
		t.Fatalf("response supersedes = %+v, want skipped thread_changed", resp.Supersedes)
	}
}

func TestCreateComment_SupersedeRejectsCommentFromAnotherIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSupersedeFixture(t)
	other := newSupersedeFixture(t)
	foreign := other.insert(t, "agent", nil, -time.Hour, "foreign plan")

	w, _ := fx.post(t, map[string]any{"content": "plan rev-2", "supersedes": []string{foreign}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create = %d: %s, want 400", w.Code, w.Body.String())
	}
	if commentResolved(t, foreign) {
		t.Fatalf("foreign thread must not be resolved")
	}
}

func TestListComments_ReturnsSupersessionLinks(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSupersedeFixture(t)
	plan1 := fx.insert(t, "agent", nil, -3*time.Hour, "plan rev-1")
	_, resp := fx.post(t, map[string]any{"content": "plan rev-2", "supersedes": []string{plan1}})

	w := httptest.NewRecorder()
	r := newRequest(http.MethodGet, "/api/issues/"+fx.IssueID+"/comments", nil)
	r = withURLParam(r, "id", fx.IssueID)
	testHandler.ListComments(w, r)
	var list []CommentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	byID := map[string]CommentResponse{}
	for _, c := range list {
		byID[c.ID] = c
	}
	if got := byID[plan1].SupersededByID; got == nil || *got != resp.ID {
		t.Fatalf("old comment superseded_by_id = %v, want %s", got, resp.ID)
	}
	if got := byID[resp.ID].Supersedes; len(got) != 1 || got[0].CommentID != plan1 {
		t.Fatalf("new comment supersedes = %+v, want plan1", got)
	}
}
