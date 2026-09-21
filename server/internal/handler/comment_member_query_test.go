package handler

import (
	"context"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestListMemberCommentsForIssue_OldGrantSurvivesCap is the CHE-556
// regression: ListCommentsForIssue keeps the NEWEST N comments and drops the
// OLDEST when its cap fires (see comment.sql's doc), so a waiver grant —
// which by construction predates the run citing it — is exactly the row a
// newest-N cap discards on an issue with more comments than the cap. This
// test seeds more member comments than the query's LIMIT and asserts the
// earliest one (the "grant") is still returned, proving
// ListMemberCommentsForIssue is oldest-first-capped rather than reusing the
// newest-N window.
func TestListMemberCommentsForIssue_OldGrantSurvivesCap(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("requires database")
	}
	issueID := createIssueForTimeline(t, "old waiver grant survives comment cap")

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	grantID := seedCommentRow(t, issueID, base, "You can skip the CI gate for this one, I already verified manually.", nil, nil)

	// Seed enough newer member comments to exceed a small cap, so a
	// newest-N-capped read would push the grant out entirely.
	const cap = 5
	for i := 1; i <= cap+3; i++ {
		seedCommentRow(t, issueID, base.Add(time.Duration(i)*time.Minute), "later chatter", nil, nil)
	}

	rows, err := testHandler.Queries.ListMemberCommentsForIssue(context.Background(), db.ListMemberCommentsForIssueParams{
		IssueID:     parseUUID(issueID),
		WorkspaceID: parseUUID(testWorkspaceID),
		Limit:       cap,
	})
	if err != nil {
		t.Fatalf("ListMemberCommentsForIssue: %v", err)
	}
	if len(rows) != cap {
		t.Fatalf("got %d rows, want exactly the cap (%d)", len(rows), cap)
	}
	if uuidToString(rows[0].ID) != grantID {
		t.Fatalf("rows[0].ID = %s, want the oldest comment (the grant, %s) — cap must discard the newest rows, not the oldest",
			uuidToString(rows[0].ID), grantID)
	}
}

// TestListMemberCommentsForIssue_ExcludesNonMemberAuthors ensures the
// author_type filter runs in SQL, not just in the Go caller: an
// agent/system-authored comment must never occupy a slot in the capped
// window that a real member grant needs.
func TestListMemberCommentsForIssue_ExcludesNonMemberAuthors(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("requires database")
	}
	issueID := createIssueForTimeline(t, "member filter excludes agent comments")

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	_, err := testPool.Exec(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, created_at, updated_at)
		VALUES ($1, $2, 'agent', $3, 'agent narration, not a grant', 'comment', $4, $4)
	`, issueID, testWorkspaceID, testUserID, base)
	if err != nil {
		t.Fatalf("seed agent comment: %v", err)
	}
	memberID := seedCommentRow(t, issueID, base.Add(time.Minute), "actual member grant", nil, nil)

	rows, err := testHandler.Queries.ListMemberCommentsForIssue(context.Background(), db.ListMemberCommentsForIssueParams{
		IssueID:     parseUUID(issueID),
		WorkspaceID: parseUUID(testWorkspaceID),
		Limit:       protocolLintCommentLimit,
	})
	if err != nil {
		t.Fatalf("ListMemberCommentsForIssue: %v", err)
	}
	if len(rows) != 1 || uuidToString(rows[0].ID) != memberID {
		t.Fatalf("rows = %v, want exactly the one member-authored comment (%s)", rows, memberID)
	}
}
