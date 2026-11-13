package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5485: handoff_comment_id on a status move reaches the destination
// agent's run as context only. It never decides who the run is accountable to.

type handoffRun struct {
	TriggerCommentID string
	TriggerKind      string
	Originator       string
	Accountable      string
	Source           string
	DelegatedFrom    string
}

// moveWithHandoff moves a fresh backlog issue assigned to a ready agent to todo
// with handoff_comment_id=commentID. The move starts the assignee's run.
func moveWithHandoff(t *testing.T, issueID, commentID string, mutate func(*http.Request)) (int, string) {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{
		"status":             "todo",
		"handoff_comment_id": commentID,
	}), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	if mutate != nil {
		mutate(req)
	}
	rec := testutil.Call(t, testHandler.UpdateIssue, req)
	return rec.Code, rec.Body.String()
}

func queuedHandoffRun(t *testing.T, issueID, agentID string) handoffRun {
	t.Helper()
	var r handoffRun
	err := testPool.QueryRow(context.Background(), `
		SELECT COALESCE(trigger_comment_id::text, ''), COALESCE(trigger_kind, ''),
		       COALESCE(originator_user_id::text, ''), COALESCE(accountable_user_id::text, ''),
		       COALESCE(originator_source, ''), COALESCE(delegated_from_task_id::text, '')
		FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
		ORDER BY created_at DESC LIMIT 1
	`, issueID, agentID).Scan(&r.TriggerCommentID, &r.TriggerKind, &r.Originator, &r.Accountable, &r.Source, &r.DelegatedFrom)
	if err != nil {
		t.Fatalf("load queued run for agent %s: %v", agentID, err)
	}
	return r
}

func backlogIssueFor(t *testing.T, title, agentID string) string {
	t.Helper()
	issueID := dbfx.Issue(t, title, testutil.Cols{
		"status":        "backlog",
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})
	return issueID
}

func runningTaskFor(t *testing.T, agentID, originator string) string {
	t.Helper()
	cols := testutil.Cols{
		"runtime_id": handlerTestRuntimeID(t),
		"status":     "running",
		"started_at": testutil.Raw("now()"),
	}
	if originator != "" {
		cols["originator_user_id"] = originator
		cols["accountable_user_id"] = originator
	}
	return dbfx.Task(t, agentID, cols)
}

// Acceptance 1 and 3: a member moves the issue with a comment another member
// wrote. The run carries the comment as status_handoff context and stays
// direct_human for the mover.
func TestStatusHandoffComment_MemberMove_RunCarriesContext(t *testing.T) {
	_, m1, m2 := privateAgentTestFixture(t)
	agentID := createHandlerTestAgent(t, "che5485-dest-member", nil)
	issueID := backlogIssueFor(t, "che5485 member move", agentID)
	comment := dbfx.Comment(t, issueID, "step brief from M1", testutil.Cols{"author_id": m1})

	code, body := moveWithHandoff(t, issueID, comment, func(r *http.Request) { r.Header.Set("X-User-ID", m2) })
	if code != http.StatusOK {
		t.Fatalf("move = %d: %s", code, body)
	}

	run := queuedHandoffRun(t, issueID, agentID)
	if run.TriggerCommentID != comment || run.TriggerKind != "status_handoff" {
		t.Errorf("run trigger = (%q, %q), want (%s, status_handoff)", run.TriggerCommentID, run.TriggerKind, comment)
	}
	if run.Originator != m2 || run.Accountable != m2 || run.Source != "direct_human" {
		t.Errorf("run attribution = %+v, want direct_human for mover %s (not comment author %s)", run, m2, m1)
	}
}

// Acceptance 2: an agent moves the issue with M1's older comment. M1 never
// becomes the run's human; the source is the mover's acting task, and the
// run cannot reach a target only M1 may invoke.
func TestStatusHandoffComment_AgentMove_DelegationNotCommentAuthor(t *testing.T) {
	privateAgentID, m1, m2 := privateAgentTestFixture(t)
	moverID := createHandlerTestAgent(t, "che5485-mover", nil)

	for _, tc := range []struct {
		name       string
		taskOrigin string
	}{
		{"acting task originated by M2", m2},
		{"acting task without a human", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destID := createHandlerTestAgent(t, "che5485-dest-agent", nil)
			issueID := backlogIssueFor(t, "che5485 agent move", destID)
			comment := dbfx.Comment(t, issueID, "older comment by M1", testutil.Cols{"author_id": m1})
			actingTask := runningTaskFor(t, moverID, tc.taskOrigin)

			code, body := moveWithHandoff(t, issueID, comment, func(r *http.Request) {
				r.Header.Del("X-Actor-Source")
				r.Header.Set("X-Agent-ID", moverID)
				r.Header.Set("X-Task-ID", actingTask)
			})
			if code != http.StatusOK {
				t.Fatalf("move = %d: %s", code, body)
			}

			run := queuedHandoffRun(t, issueID, destID)
			if run.TriggerCommentID != comment || run.TriggerKind != "status_handoff" {
				t.Errorf("run trigger = (%q, %q), want (%s, status_handoff)", run.TriggerCommentID, run.TriggerKind, comment)
			}
			if run.Originator == m1 || run.Accountable == m1 {
				t.Errorf("run attributed to comment author M1: %+v", run)
			}
			if run.Source == "comment_source" {
				t.Errorf("run source = comment_source, want the mover's acting task or the issue fallback")
			}
			if tc.taskOrigin != "" && (run.Originator != tc.taskOrigin || run.Source != "delegation" || run.DelegatedFrom != actingTask) {
				t.Errorf("run = %+v, want delegation from acting task %s for %s", run, actingTask, tc.taskOrigin)
			}

			// A target only M1 may invoke refuses the run's originator.
			target, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(privateAgentID))
			if err != nil {
				t.Fatal(err)
			}
			if testHandler.canInvokeAgent(context.Background(), target, "agent", moverID, run.Originator, testWorkspaceID) {
				t.Errorf("canInvokeAgent admitted originator %q to M1's private agent", run.Originator)
			}
		})
	}
}

// Acceptance 4: a comment that is deleted, on another issue or in another
// workspace is refused, and no run starts.
func TestStatusHandoffComment_BadComment_Refused(t *testing.T) {
	agentID := createHandlerTestAgent(t, "che5485-dest-refuse", nil)
	other := dbfx.Issue(t, "che5485 other issue")

	otherWS := dbfx.Insert(t, "workspace", testutil.Cols{"name": "che5485-ws", "slug": "che5485-ws", "issue_prefix": "C55"})
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, otherWS) })
	otherWSIssue := dbfx.Issue(t, "che5485 foreign issue", testutil.Cols{"workspace_id": otherWS, "number": 1})

	cases := map[string]func(issueID string) string{
		"other issue": func(string) string { return dbfx.Comment(t, other, "elsewhere") },
		"other workspace": func(string) string {
			return dbfx.Comment(t, otherWSIssue, "foreign", testutil.Cols{"workspace_id": otherWS})
		},
		"deleted": func(issueID string) string {
			id := dbfx.Comment(t, issueID, "gone")
			dbfx.Exec(t, `UPDATE comment SET deleted_at = now() WHERE id = $1`, id)
			return id
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			issueID := backlogIssueFor(t, "che5485 refuse "+name, agentID)
			code, body := moveWithHandoff(t, issueID, mk(issueID), nil)
			if code != http.StatusBadRequest || !strings.Contains(body, "comment_not_on_issue") {
				t.Fatalf("move = %d %s, want 400 comment_not_on_issue", code, body)
			}
			var queued int
			dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&queued)
			if queued != 0 {
				t.Errorf("queued runs = %d, want 0", queued)
			}
		})
	}
}
