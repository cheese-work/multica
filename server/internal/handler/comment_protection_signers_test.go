package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The signer list is only as trustworthy as the comments it reads. These two
// tests tie the VerifiedSigners service to the real comment handlers (CHE-10267).

func TestCommentProtectionOnReportsTheS1Rule(t *testing.T) {
	if !CommentProtectionOn() {
		t.Fatal("CommentProtectionOn() = false, want true: an agent run must not modify another agent's comment")
	}
}

func TestAnotherAgentsDeleteOfAFailIsRefusedSoTheFailStillWithdrawsThePass(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	issueID := dbfx.Issue(t, "verified signers survive a delete")
	reviewer := createHandlerTestAgent(t, "signers-reviewer", nil)
	reviewerTask := createHandlerTestTaskForAgentOnIssue(t, reviewer, issueID)
	attacker := createHandlerTestAgent(t, "signers-attacker", nil)
	attackerTask := createHandlerTestTaskForAgentOnIssue(t, attacker, issueID)

	const head = "abc123"
	verdict := func(word string, at time.Time) string {
		return dbfx.Comment(t, issueID, "**Verdict: "+word+"** — PR #9, head `"+head+"`.\n\nReviewer: reviewer-one.", testutil.Cols{
			"author_type": "agent", "author_id": reviewer, "source_task_id": reviewerTask, "created_at": at,
		})
	}
	now := time.Now()
	verdict("PASS", now.Add(-2*time.Hour))
	fail := verdict("FAIL", now.Add(-time.Hour))

	req := asAgent(newRequest(http.MethodDelete, "/api/comments/"+fail, nil), attacker, attackerTask)
	deleteComment(t, req, fail).Want(http.StatusForbidden)

	var ws, issue pgtype.UUID
	if err := ws.Scan(testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := issue.Scan(issueID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	got, err := service.VerifiedSigners(ctx, tx, db.New(testPool), service.MergeCandidate{WorkspaceID: ws, IssueID: issue, HeadSHA: head}, CommentProtectionOn)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Signers) != 0 {
		t.Fatalf("signers = %+v, want none: the refused delete leaves the FAIL standing", got.Signers)
	}
	if !got.CommentProtection {
		t.Fatal("comment_protection = false, want true with the real S1 helper")
	}
}
