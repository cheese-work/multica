package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5489 (S2): a status move can create its handoff comment, and name
// existing comments, inside the status transaction. The comment is context for
// the handoff run; it starts no comment-triggered run of its own.

type commentRun struct {
	TriggerCommentID string
	TriggerKind      string
	Coalesced        []string
}

func runsOf(t *testing.T, issueID, agentID string) []commentRun {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT COALESCE(trigger_comment_id::text, ''), COALESCE(trigger_kind, ''),
		       COALESCE(ARRAY(SELECT unnest(coalesced_comment_ids)::text), '{}')
		FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 ORDER BY created_at`, issueID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []commentRun
	for rows.Next() {
		var r commentRun
		if err := rows.Scan(&r.TriggerCommentID, &r.TriggerKind, &r.Coalesced); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func commentIDsOn(t *testing.T, issueID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id::text FROM comment WHERE issue_id = $1 ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func moveWithHandoffBody(t *testing.T, issueID string, extra map[string]any) *testutil.Response {
	t.Helper()
	body := map[string]any{"status": "in_progress"}
	for k, v := range extra {
		body[k] = v
	}
	return issueWriters[0].write(t, issueID, body, map[string]string{"X-Actor-Source": "interactive_jwt"})
}

// Acceptance 1: the move creates the comment and one handoff run. Neither the
// old nor the new assignee gets a comment-triggered run.
func TestHandoffComment_CreatedInMove_OneHandoffRunNoCommentRun(t *testing.T) {
	issueID, destID := handoffDispatchFixture(t)
	oldID := createHandlerTestAgent(t, "che5489-old-assignee", nil)
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, oldID)

	moveWithHandoffBody(t, issueID, map[string]any{"handoff_comment": map[string]any{"content": "please review the migration"}}).Want(http.StatusOK)

	comments := commentIDsOn(t, issueID)
	if len(comments) != 1 {
		t.Fatalf("comments on the issue = %d, want 1 (the handoff comment)", len(comments))
	}
	runs := runsOf(t, issueID, destID)
	if len(runs) != 1 || runs[0].TriggerCommentID != comments[0] || runs[0].TriggerKind != "status_handoff" {
		t.Fatalf("destination runs = %+v, want one status_handoff run on comment %s", runs, comments[0])
	}
	if old := runsOf(t, issueID, oldID); len(old) != 0 {
		t.Errorf("previous assignee runs = %+v, want none", old)
	}
}

// Acceptance 2: a third agent named in the new comment gets its own run, as a
// comment trigger under the comment's author.
func TestHandoffComment_MentionedThirdAgentGetsOwnRun(t *testing.T) {
	issueID, destID := handoffDispatchFixture(t)
	thirdID := createHandlerTestAgent(t, "che5489-third", nil)

	moveWithHandoffBody(t, issueID, map[string]any{"handoff_comment": map[string]any{
		"content": "[@Third](mention://agent/" + thirdID + ") check this, then it is yours",
	}}).Want(http.StatusOK)

	comments := commentIDsOn(t, issueID)
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(comments))
	}
	third := runsOf(t, issueID, thirdID)
	if len(third) != 1 || third[0].TriggerCommentID != comments[0] || third[0].TriggerKind == "status_handoff" {
		t.Errorf("third agent runs = %+v, want one comment-triggered run on %s", third, comments[0])
	}
	if dest := runsOf(t, issueID, destID); len(dest) != 1 || dest[0].TriggerKind != "status_handoff" {
		t.Errorf("destination runs = %+v, want exactly one status_handoff run", dest)
	}
}

// Acceptance 3: attach_comment_ids keeps its order on the run: the first id is
// the trigger comment, the rest are coalesced, in the given order.
func TestHandoffComment_AttachOrderKeptOnRun(t *testing.T) {
	issueID, destID := handoffDispatchFixture(t)
	a := dbfx.Comment(t, issueID, "first written")
	b := dbfx.Comment(t, issueID, "second written")
	c := dbfx.Comment(t, issueID, "third written")

	moveWithHandoffBody(t, issueID, map[string]any{"attach_comment_ids": []string{c, a, b}}).Want(http.StatusOK)

	runs := runsOf(t, issueID, destID)
	if len(runs) != 1 {
		t.Fatalf("destination runs = %+v, want 1", runs)
	}
	if runs[0].TriggerCommentID != c || len(runs[0].Coalesced) != 2 || runs[0].Coalesced[0] != a || runs[0].Coalesced[1] != b {
		t.Errorf("run = %+v, want trigger %s and coalesced [%s %s]", runs[0], c, a, b)
	}
}

// Acceptance 4: a stale preview or a foreign comment refuses the whole move.
// Nothing is written: no comment, no status change, no run.
func TestHandoffComment_StaleOrForeignRefusedNothingWritten(t *testing.T) {
	other := dbfx.Issue(t, "che5489 other issue")
	foreign := dbfx.Comment(t, other, "elsewhere")
	deleted := ""
	cases := map[string]struct {
		extra func(issueID string) map[string]any
		code  int
		want  string
	}{
		"stale preview": {func(string) map[string]any {
			return map[string]any{
				"handoff_comment": map[string]any{"content": "stale"},
				"handoff_preview": map[string]any{"handler_type": "agent", "handler_id": "00000000-0000-0000-0000-000000000001"},
			}
		}, http.StatusConflict, "stale_preview"},
		"foreign comment": {func(string) map[string]any {
			return map[string]any{"handoff_comment": map[string]any{"content": "x"}, "attach_comment_ids": []string{foreign}}
		}, http.StatusBadRequest, "comment_not_on_issue"},
		"deleted comment": {func(issueID string) map[string]any {
			deleted = dbfx.Comment(t, issueID, "gone")
			dbfx.Exec(t, `UPDATE comment SET deleted_at = now() WHERE id = $1`, deleted)
			return map[string]any{"handoff_comment": map[string]any{"content": "x"}, "attach_comment_ids": []string{deleted}}
		}, http.StatusBadRequest, "comment_not_on_issue"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			issueID, destID := handoffDispatchFixture(t)
			before := len(commentIDsOn(t, issueID))
			extra := tc.extra(issueID)
			if name == "deleted comment" {
				before = len(commentIDsOn(t, issueID))
			}
			resp := moveWithHandoffBody(t, issueID, extra)
			if resp.Code != tc.code || !strings.Contains(resp.Body.String(), tc.want) {
				t.Fatalf("move = %d %s, want %d %s", resp.Code, resp.Body.String(), tc.code, tc.want)
			}
			if after := len(commentIDsOn(t, issueID)); after != before {
				t.Errorf("comments = %d, want %d: a refused move writes no comment", after, before)
			}
			wantIssueStatus(t, issueID, "todo")
			if runs := runsOf(t, issueID, destID); len(runs) != 0 {
				t.Errorf("runs = %+v, want none", runs)
			}
		})
	}
}

func previewFor(destID string, revisions map[string]int64) map[string]any {
	return map[string]any{
		"previous_assignee_type": nil, "previous_assignee_id": nil,
		"handler_type": "agent", "handler_id": destID, "comment_revisions": revisions,
	}
}

func bumpCommentRevision(t *testing.T, commentID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE comment SET revision = revision + 1 WHERE id = $1`, commentID)
}

// A comment edited after the preview was shown is not what the person
// confirmed: the whole move is refused and nothing is written.
func TestHandoffComment_AttachedCommentEditedAfterPreviewRefused(t *testing.T) {
	issueID, destID := handoffDispatchFixture(t)
	a := dbfx.Comment(t, issueID, "as previewed")
	bumpCommentRevision(t, a)

	resp := moveWithHandoffBody(t, issueID, map[string]any{
		"handoff_comment":    map[string]any{"content": "x"},
		"attach_comment_ids": []string{a},
		"handoff_preview":    previewFor(destID, map[string]int64{a: 1}),
	})
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "stale_comment") {
		t.Fatalf("move = %d %s, want 409 stale_comment", resp.Code, resp.Body.String())
	}
	if got := commentIDsOn(t, issueID); len(got) != 1 {
		t.Errorf("comments = %d, want 1: a refused move writes no comment", len(got))
	}
	wantIssueStatus(t, issueID, "todo")
	if runs := runsOf(t, issueID, destID); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
}

// A preview names a revision for every comment it attaches.
func TestHandoffComment_PreviewWithoutRevisionForAttachedRefused(t *testing.T) {
	issueID, destID := handoffDispatchFixture(t)
	a := dbfx.Comment(t, issueID, "no revision named")

	resp := moveWithHandoffBody(t, issueID, map[string]any{
		"attach_comment_ids": []string{a},
		"handoff_preview":    previewFor(destID, map[string]int64{}),
	})
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "comment_revision_required") {
		t.Fatalf("move = %d %s, want 400 comment_revision_required", resp.Code, resp.Body.String())
	}
	wantIssueStatus(t, issueID, "todo")
}

// The revision the preview named still matches: the move goes through.
func TestHandoffComment_AttachedRevisionMatchesAccepted(t *testing.T) {
	issueID, destID := handoffDispatchFixture(t)
	a := dbfx.Comment(t, issueID, "as previewed")

	moveWithHandoffBody(t, issueID, map[string]any{
		"attach_comment_ids": []string{a},
		"handoff_preview":    previewFor(destID, map[string]int64{a: 1}),
	}).Want(http.StatusOK)
	if runs := runsOf(t, issueID, destID); len(runs) != 1 || runs[0].TriggerCommentID != a {
		t.Fatalf("runs = %+v, want one run on %s", runs, a)
	}
}

// The comment bumps the issue revision. The move returns the revision it
// committed, so the next write that names it is not a conflict.
func TestHandoffComment_ReturnsCommittedIssueRevision(t *testing.T) {
	issueID, _ := handoffDispatchFixture(t)

	var moved struct {
		Revision int64 `json:"revision"`
	}
	moveWithHandoffBody(t, issueID, map[string]any{"handoff_comment": map[string]any{"content": "done here"}}).
		Want(http.StatusOK).JSON(&moved)

	var stored int64
	if err := testPool.QueryRow(context.Background(), `SELECT revision FROM issue WHERE id = $1`, issueID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if moved.Revision != stored {
		t.Fatalf("response revision = %d, stored = %d", moved.Revision, stored)
	}
	issueWriters[0].write(t, issueID, map[string]any{"title": "renamed", "expected_revision": moved.Revision}, nil).Want(http.StatusOK)
}

// The status transaction checks the attached comments again under the issue
// lock: a comment deleted or edited between the first check and the commit
// refuses the move. A retrospective test (added after the code): the race
// window cannot be held open through HTTP.
func TestVerifyAttachedComments_UnderLock(t *testing.T) {
	issueID, _ := handoffDispatchFixture(t)
	q := testHandler.Queries
	issue, err := q.GetIssueInWorkspace(context.Background(), db.GetIssueInWorkspaceParams{ID: parseUUID(issueID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	live := dbfx.Comment(t, issueID, "live")
	gone := dbfx.Comment(t, issueID, "gone")
	dbfx.Exec(t, `UPDATE comment SET deleted_at = now() WHERE id = $1`, gone)
	edited := dbfx.Comment(t, issueID, "edited")
	bumpCommentRevision(t, edited)
	foreign := dbfx.Comment(t, dbfx.Issue(t, "che5489 verify other"), "elsewhere")

	one := func(id string, rev int64) *attachedComments {
		return &attachedComments{ids: []pgtype.UUID{parseUUID(id)}, revisions: map[pgtype.UUID]int64{parseUUID(id): rev}}
	}
	for name, tc := range map[string]struct {
		in   *attachedComments
		want error
	}{
		"live at its revision": {one(live, 1), nil},
		"deleted":              {one(gone, 1), errAttachedCommentGone},
		"foreign issue":        {one(foreign, 1), errAttachedCommentGone},
		"edited":               {one(edited, 1), errAttachedCommentStale},
		"unbound live":         {&attachedComments{ids: []pgtype.UUID{parseUUID(live)}}, nil},
	} {
		if got := verifyAttachedComments(context.Background(), q, issue, tc.in); !errors.Is(got, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, got, tc.want)
		}
	}
}
