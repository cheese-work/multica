package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5838: a member-owned open issue with no open decision card goes back to
// the last agent that worked on it, and that agent is woken. An open card is the
// legitimate human wait and stays.
type memberSweepFixture struct{ agent, issue string }

func newMemberOwnedIssue(t *testing.T, status, tag string) memberSweepFixture {
	t.Helper()
	agent := createHandlerTestAgent(t, "member sweep "+t.Name()+tag, nil)
	issue := dbfx.Issue(t, "member-owned "+t.Name(), testutil.Cols{
		"status": status, "assignee_type": "member", "assignee_id": testUserID,
	})
	// The agent's earlier, finished run is how the sweep finds the last agent.
	dbfx.Task(t, agent, testutil.Cols{
		"runtime_id": handlerTestRuntimeID(t), "status": "completed", "issue_id": issue,
		"started_at": testutil.Raw("now() - interval '2 hours'"), "completed_at": testutil.Raw("now() - interval '1 hour'"),
	})
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", issue)
	dbfx.Cleanup(t, "DELETE FROM comment WHERE issue_id = $1", issue)
	return memberSweepFixture{agent: agent, issue: issue}
}

func (f memberSweepFixture) sweep(t *testing.T) {
	t.Helper()
	if err := testHandler.SweepMemberOwnedIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f memberSweepFixture) queuedRuns(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, f.issue, f.agent)
}

func (f memberSweepFixture) notes(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE 'Member-owned sweep:%'`, f.issue)
}

func TestMemberOwnedSweepReturnsIssueToLastAgentAndWakesIt_CHE5838(t *testing.T) {
	f := newMemberOwnedIssue(t, "in_review", "")
	f.sweep(t)
	status, assigneeType, assigneeID := issueStatusAndAssignee(t, f.issue)
	if assigneeType != "agent" || assigneeID != f.agent {
		t.Fatalf("assignee = %s/%s, want agent/%s", assigneeType, assigneeID, f.agent)
	}
	if status != "in_review" {
		t.Fatalf("status = %q, want it unchanged", status)
	}
	if got := f.queuedRuns(t); got != 1 {
		t.Fatalf("queued runs for the last agent = %d, want 1", got)
	}
	if got := f.notes(t); got != 1 {
		t.Fatalf("sweep notes = %d, want one line", got)
	}
	f.sweep(t)
	if f.queuedRuns(t) != 1 || f.notes(t) != 1 {
		t.Fatal("a second pass must not wake or note again")
	}
}

func TestMemberOwnedSweepLeavesIssueWithOpenCard_CHE5838(t *testing.T) {
	f := newMemberOwnedIssue(t, "need_decision", "")
	dbfx.QueryRow(t, `INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ask, escalation_class, origin)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), 'Pick', 'reversible', 'asked') RETURNING id::text`,
		testWorkspaceID, f.issue, f.agent, testUserID).Scan(new(string))
	f.sweep(t)
	if _, assigneeType, _ := issueStatusAndAssignee(t, f.issue); assigneeType != "member" {
		t.Fatalf("assignee_type = %q, want the issue to stay with the member", assigneeType)
	}
	if f.queuedRuns(t) != 0 || f.notes(t) != 0 {
		t.Fatal("an issue with an open card must get no run and no note")
	}
}

func TestMemberOwnedSweepSkipsClosedAndAgentlessIssues_CHE5838(t *testing.T) {
	done := newMemberOwnedIssue(t, "done", "")
	agentless := newMemberOwnedIssue(t, "in_review", " agentless")
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, agentless.issue)
	done.sweep(t)
	for _, f := range []memberSweepFixture{done, agentless} {
		if _, assigneeType, _ := issueStatusAndAssignee(t, f.issue); assigneeType != "member" {
			t.Fatalf("issue %s assignee_type = %q, want member", f.issue, assigneeType)
		}
		if f.queuedRuns(t) != 0 || f.notes(t) != 0 {
			t.Fatalf("issue %s must get no run and no note", f.issue)
		}
	}
}
