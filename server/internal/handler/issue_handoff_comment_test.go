package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
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
