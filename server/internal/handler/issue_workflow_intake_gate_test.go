package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7668: the intake_simple and qa_proof entry guards, through the status write.

func intakeSteps(mode string) []map[string]any {
	g := gate("intake_simple", mode)
	g["param"] = "1"
	return []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo"},
		{"status_key": "investigating"},
		{"status_key": "in_progress", "entry": []map[string]any{g}},
		{"status_key": "blocked"},
	}
}

func qaSteps(mode string) []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo"},
		{"status_key": "in_progress"},
		{"status_key": "agent_review", "entry": []map[string]any{gate("qa_proof", mode)}},
		{"status_key": "blocked"},
	}
}

const simpleIntakeComment = "Intake\nEstimate: 1h\nRepos: 1\nSchema change: no\nSecurity change: no\nHuman decision: no"

func addGateComment(t *testing.T, issueID, authorType, authorID, content, at string) {
	t.Helper()
	dbfx.Insert(t, "comment", testutil.Cols{"issue_id": issueID, "workspace_id": testWorkspaceID, "author_type": authorType, "author_id": authorID, "content": content, "created_at": at})
}

// markEntered stores the move that put the issue on its current step.
func markEntered(t *testing.T, issueID, status, at string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details, created_at) VALUES ($1,$2,'member','status_changed',jsonb_build_object('from','todo','to',$3::text),$4)`,
		testWorkspaceID, issueID, status, at)
}

func intakeCalls(t *testing.T, issueID string) (n int, call, estimate string) {
	t.Helper()
	n = activityCount(t, issueID, "intake_call")
	if n > 0 {
		dbfx.QueryRow(t, `SELECT details->>'call', COALESCE(details->>'estimate','') FROM activity_log WHERE issue_id=$1 AND action='intake_call' ORDER BY created_at DESC, id DESC LIMIT 1`, issueID).Scan(&call, &estimate)
	}
	return n, call, estimate
}

func TestIntakeSimpleGateBlocksAndLogsRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 7668001, intakeSteps("block"))
	c.enter(t, "investigating")
	addGateComment(t, c.issueID, "member", testUserID, "Intake\nEstimate: 3h\nRepos: 1\nSchema change: no\nSecurity change: no\nHuman decision: no", "2026-01-01T00:00:00Z")

	res := testutil.Call(t, testHandler.UpdateIssue, asAgentTask(t, withURLParam(newRequest("PUT", "/api/issues/"+c.issueID, map[string]any{"status": "in_progress"}), "id", c.issueID)))
	res.Want(http.StatusConflict)
	if got := failedGuards(t, res); len(got) != 1 || got[0] != "intake_simple" {
		t.Fatalf("failed guards = %v; body %s", got, res.Text())
	}
	wantIssueStatus(t, c.issueID, "investigating")
	if n, call, est := intakeCalls(t, c.issueID); n != 1 || call != "rejected" || est != "3h" {
		t.Fatalf("intake_call rows=%d call=%q estimate=%q, want one rejected 3h even though the write was refused", n, call, est)
	}
}

func TestIntakeSimpleGatePassesASimpleIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 7668002, intakeSteps("block"))
	c.enter(t, "investigating")
	addGateComment(t, c.issueID, "agent", handlerTestAgentID(t), simpleIntakeComment, "2026-01-01T00:00:00Z")

	testutil.Call(t, testHandler.UpdateIssue, asAgentTask(t, withURLParam(newRequest("PUT", "/api/issues/"+c.issueID, map[string]any{"status": "in_progress"}), "id", c.issueID))).Want(http.StatusOK)
	wantIssueStatus(t, c.issueID, "in_progress")
	if n, call, est := intakeCalls(t, c.issueID); n != 1 || call != "simple" || est != "1h" {
		t.Fatalf("intake_call rows=%d call=%q estimate=%q, want one simple 1h", n, call, est)
	}
}

func TestIntakeSimpleGateReadsTheIssueDescriptionAndNewestComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 7668003, intakeSteps("block"))
	c.enter(t, "investigating")
	dbfx.Exec(t, `UPDATE issue SET description=$2 WHERE id=$1`, c.issueID, "Estimate: 5h\nRepos: 1\nSchema change: no\nSecurity change: no\nHuman decision: no")
	addGateComment(t, c.issueID, "agent", handlerTestAgentID(t), "Estimate: 1h", "2026-01-01T00:00:00Z")

	testutil.Call(t, testHandler.UpdateIssue, asAgentTask(t, withURLParam(newRequest("PUT", "/api/issues/"+c.issueID, map[string]any{"status": "in_progress"}), "id", c.issueID))).Want(http.StatusOK)
}

func TestIntakeSimpleGateOpenDecisionRequestFails(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 7668004, intakeSteps("block"))
	c.enter(t, "investigating")
	addGateComment(t, c.issueID, "agent", handlerTestAgentID(t), simpleIntakeComment, "2026-01-01T00:00:00Z")
	id := dbfx.Insert(t, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": c.issueID, "asking_agent_id": handlerTestAgentID(t), "human_user_id": testUserID, "ask": "Pick?", "escalation_class": "reversible", "origin": "asked", "status": "open"})
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM decision_request WHERE id=$1`, id) })

	res := testutil.Call(t, testHandler.UpdateIssue, asAgentTask(t, withURLParam(newRequest("PUT", "/api/issues/"+c.issueID, map[string]any{"status": "in_progress"}), "id", c.issueID)))
	res.Want(http.StatusConflict)
	if n, call, _ := intakeCalls(t, c.issueID); n != 1 || call != "rejected" {
		t.Fatalf("intake_call rows=%d call=%q, want one rejected", n, call)
	}
}

func TestIntakeSimpleGateModes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, tc := range []struct {
		mode      string
		want      int
		wantCalls int
		warnings  int
	}{
		{"off", http.StatusOK, 0, 0},
		{"warn", http.StatusOK, 1, 1},
		{"block", http.StatusConflict, 1, 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c := newEdgeCase(t, int64(7668010+i), intakeSteps(tc.mode))
			c.enter(t, "investigating")
			addGateComment(t, c.issueID, "agent", handlerTestAgentID(t), "Estimate: 1h\nRepos: 2\nSchema change: no\nSecurity change: no\nHuman decision: no", "2026-01-01T00:00:00Z")
			testutil.Call(t, testHandler.UpdateIssue, asAgentTask(t, withURLParam(newRequest("PUT", "/api/issues/"+c.issueID, map[string]any{"status": "in_progress"}), "id", c.issueID))).Want(tc.want)
			if n, _, _ := intakeCalls(t, c.issueID); n != tc.wantCalls {
				t.Fatalf("intake_call rows = %d, want %d", n, tc.wantCalls)
			}
			if n := activityCount(t, c.issueID, "entry_gate_warning"); n != tc.warnings {
				t.Fatalf("warning rows = %d, want %d", n, tc.warnings)
			}
		})
	}
}

func TestIntakeSimpleGateMemberOverride(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 7668020, intakeSteps("block"))
	c.enter(t, "investigating")
	addGateComment(t, c.issueID, "member", testUserID, "Estimate: 1h\nRepos: 2\nSchema change: no\nSecurity change: no\nHuman decision: no", "2026-01-01T00:00:00Z")

	putIssue(t, c.issueID, map[string]any{"status": "in_progress"}).Want(http.StatusConflict)
	putIssue(t, c.issueID, map[string]any{"status": "in_progress", "entry_gate_override_reason": "two repos, one is docs only"}).Want(http.StatusOK)
	if n := activityCount(t, c.issueID, "entry_gate_override"); n != 1 {
		t.Fatalf("override rows = %d, want 1", n)
	}
	if n, call, _ := intakeCalls(t, c.issueID); n != 2 || call != "rejected" {
		t.Fatalf("intake_call rows=%d call=%q: every evaluation writes one, both rejected", n, call)
	}
}

func qaWrite(t *testing.T, c edgeCase, actor ...func(*http.Request) *http.Request) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("PUT", "/api/issues/"+c.issueID, map[string]any{"status": "agent_review"}), "id", c.issueID)
	return testutil.Call(t, testHandler.UpdateIssue, asAgentTask(t, req))
}

func TestQAProofGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const entered = "2026-01-02T00:00:00Z"
	agent := handlerTestAgentID(t)
	for i, tc := range []struct {
		name  string
		setup func(t *testing.T, issueID string)
		want  int
	}{
		{"url proof and post_close_work", func(t *testing.T, id string) {
			addGateComment(t, id, "agent", agent, "Done.\nQA: https://example.com/run/1", "2026-01-02T01:00:00Z")
			dbfx.Exec(t, `UPDATE issue SET metadata=jsonb_build_object('post_close_work','none') WHERE id=$1`, id)
		}, http.StatusOK},
		{"fenced proof", func(t *testing.T, id string) {
			addGateComment(t, id, "agent", agent, "QA:\n```\nok  github.com/x/y 0.4s\n```", "2026-01-02T01:00:00Z")
			dbfx.Exec(t, `UPDATE issue SET metadata=jsonb_build_object('post_close_work','docs follow-up')WHERE id=$1`, id)
		}, http.StatusOK},
		{"missing post_close_work", func(t *testing.T, id string) {
			addGateComment(t, id, "agent", agent, "QA: https://example.com/run/1", "2026-01-02T01:00:00Z")
		}, http.StatusConflict},
		{"non-string post_close_work", func(t *testing.T, id string) {
			addGateComment(t, id, "agent", agent, "QA: https://example.com/run/1", "2026-01-02T01:00:00Z")
			dbfx.Exec(t, `UPDATE issue SET metadata=jsonb_build_object('post_close_work',7) WHERE id=$1`, id)
		}, http.StatusConflict},
		{"QA before the step was entered", func(t *testing.T, id string) {
			addGateComment(t, id, "agent", agent, "QA: https://example.com/run/1", "2026-01-01T01:00:00Z")
			dbfx.Exec(t, `UPDATE issue SET metadata=jsonb_build_object('post_close_work','none') WHERE id=$1`, id)
		}, http.StatusConflict},
		{"QA by another author", func(t *testing.T, id string) {
			addGateComment(t, id, "member", testUserID, "QA: https://example.com/run/1", "2026-01-02T01:00:00Z")
			dbfx.Exec(t, `UPDATE issue SET metadata=jsonb_build_object('post_close_work','none') WHERE id=$1`, id)
		}, http.StatusConflict},
		{"no QA at all", func(t *testing.T, id string) {
			dbfx.Exec(t, `UPDATE issue SET metadata=jsonb_build_object('post_close_work','none') WHERE id=$1`, id)
		}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEdgeCase(t, int64(7668030+i), qaSteps("block"))
			c.enter(t, "in_progress")
			markEntered(t, c.issueID, "in_progress", entered)
			tc.setup(t, c.issueID)
			res := qaWrite(t, c)
			res.Want(tc.want)
			if tc.want == http.StatusConflict {
				if got := failedGuards(t, res); len(got) != 1 || got[0] != "qa_proof" {
					t.Fatalf("failed guards = %v; body %s", got, res.Text())
				}
				wantIssueStatus(t, c.issueID, "in_progress")
			}
		})
	}
}

func TestQAProofGateWarnAndOffAndOverride(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, tc := range []struct {
		mode string
		want int
	}{{"off", http.StatusOK}, {"warn", http.StatusOK}, {"block", http.StatusConflict}} {
		t.Run(tc.mode, func(t *testing.T) {
			c := newEdgeCase(t, int64(7668040+i), qaSteps(tc.mode))
			c.enter(t, "in_progress")
			qaWrite(t, c).Want(tc.want)
		})
	}
	c := newEdgeCase(t, 7668045, qaSteps("block"))
	c.enter(t, "in_progress")
	putIssue(t, c.issueID, map[string]any{"status": "agent_review", "entry_gate_override_reason": "proof is in the PR"}).Want(http.StatusOK)
}
