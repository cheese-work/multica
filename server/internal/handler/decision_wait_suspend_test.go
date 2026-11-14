package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// exhaustBudget makes the next run for the fixture's issue exceed the cap: the
// completed run after the window start is the one run allowed (a person's move
// restarts the window, so the run is created after the caller's moves). The
// ask's own ledger entries (the window and the step grant) are dropped so the
// new window starts from the cap.
func (f episodeFixture) exhaustBudget(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `DELETE FROM issue_run_budget_entry WHERE issue_id = $1`, f.issueID)
	// Another agent's run, so the asking agent's own runs stay countable.
	other := dbfx.Agent(t, "Budget filler "+workflowTestSuffix(), handlerTestRuntimeID(t))
	dbfx.Task(t, other, testutil.Cols{"issue_id": f.issueID, "status": "completed", "runtime_id": handlerTestRuntimeID(t)})
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 1, run_budget_since = now() - interval '1 hour'
		WHERE id = (SELECT workflow_id FROM project WHERE id = (SELECT project_id FROM issue WHERE id = $1))`, f.issueID)
}

// conflictingTask is an active task of the asking agent whose squad context
// differs from the episode's, so the dispatch has no legal target (design §4.4.4).
func (f episodeFixture) conflictingTask(t *testing.T) string {
	t.Helper()
	squad := dbfx.Insert(t, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Conflicting squad " + workflowTestSuffix(), "creator_id": testUserID, "leader_id": f.agentID})
	return dbfx.Task(t, f.agentID, testutil.Cols{"issue_id": f.issueID, "status": "queued", "runtime_id": handlerTestRuntimeID(t), "squad_id": squad, "is_leader_task": true})
}

// suspension is the episode's suspended state and the issue placement around it.
type suspension struct {
	phase, reason, refusedTask, restoreStatus string
	flag                                      bool
	suspendedAt                               time.Time
	issueStatus, blockedReason                string
}

func (f episodeFixture) suspension(t *testing.T) suspension {
	t.Helper()
	var s suspension
	dbfx.QueryRow(t, `SELECT w.phase, COALESCE(w.suspend_reason, ''), COALESCE(w.refused_task_id::text, ''), w.restore_status, w.restore_on_resume,
			COALESCE(w.suspended_at, 'epoch'), i.status, COALESCE(i.metadata ->> 'blocked_reason', '')
		FROM decision_wait w JOIN issue i ON i.id = w.issue_id WHERE w.issue_id = $1 AND w.closed_at IS NULL`, f.issueID).
		Scan(&s.phase, &s.reason, &s.refusedTask, &s.restoreStatus, &s.flag, &s.suspendedAt, &s.issueStatus, &s.blockedReason)
	return s
}

// suspensionComments are the system comments the suspension write posts.
func (f episodeFixture) suspensionComments(t *testing.T) []string {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE 'Decision wait suspended%' ORDER BY created_at`, f.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatal(err)
		}
		out = append(out, content)
	}
	return out
}

// Design §4.4.3 and §4.4.7: a run-budget refusal rolls back the whole attempt
// and writes one suspension. Nothing is acknowledged or dispatched; the issue
// is blocked on the episode's own block, so the saved target stays.
func TestDecisionEpisodeRunBudgetRefusalSuspends(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	f.exhaustBudget(t)

	f.deliverEpisodeOnly(t)
	service.WaitRunBudgetHooks()

	s := f.suspension(t)
	if s.phase != "suspended" || s.reason != "run_budget" || s.refusedTask != "" || !s.flag || s.restoreStatus != "in_progress" {
		t.Fatalf("episode = %+v, want suspended run_budget with the in_progress target and the flag set", s)
	}
	if s.issueStatus != "blocked" || s.blockedReason != "run-budget" {
		t.Fatalf("issue = %s reason %q, want blocked by run-budget", s.issueStatus, s.blockedReason)
	}
	if woken, _ := f.answerState(t, answerID); woken || f.continuations(t) != 0 || len(f.closedEpisodes(t)) != 0 {
		t.Fatal("the refused attempt acknowledged an answer, dispatched, or closed the episode")
	}
	comments := f.suspensionComments(t)
	if len(comments) != 1 || !strings.Contains(comments[0], "run_budget") || !strings.Contains(comments[0], "moving the issue resumes it") {
		t.Fatalf("suspension comments = %q, want one naming run_budget and how it resumes", comments)
	}

	// Nothing changed: the episode stays suspended and writes nothing more.
	f.deliverEpisodeOnly(t)
	if got := f.suspension(t); got.phase != "suspended" || !got.suspendedAt.Equal(s.suspendedAt) || len(f.suspensionComments(t)) != 1 {
		t.Fatalf("a delivery with no resume condition changed the suspension: %+v", got)
	}

	// Raising the budget window past suspended_at resumes: restore, dispatch, acknowledge.
	dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since = clock_timestamp()
		WHERE id = (SELECT workflow_id FROM project WHERE id = (SELECT project_id FROM issue WHERE id = $1))`, f.issueID)
	f.deliverEpisodeOnly(t)
	f.wantOneContinuation(t, "answered")
	if status, assigneeType, assigneeID := issueStatusAndAssignee(t, f.issueID); status != "in_progress" || assigneeType != "agent" || assigneeID != f.agentID {
		t.Fatalf("issue = %s %s/%s, want the restored placement", status, assigneeType, assigneeID)
	}
	if woken, outcome := f.answerState(t, answerID); !woken || outcome != "run_enqueued" {
		t.Fatalf("answer woken=%v outcome=%q, want run_enqueued", woken, outcome)
	}
}

// A person's block keeps its reason and the saved target (§4.4.7 step 4, flag false).
func TestDecisionEpisodeRunBudgetRefusalKeepsAPersonsBlock(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.answerRequest(t, f.requestID, -time.Minute, false)
	f.memberMove(t, "blocked").Want(200)
	if s := f.suspension(t); s.reason != "status_moved" || s.flag {
		t.Fatalf("after the person's move = %+v, want suspended status_moved with the flag cleared", s)
	}
	f.exhaustBudget(t)

	f.deliverEpisodeOnly(t)
	service.WaitRunBudgetHooks()

	s := f.suspension(t)
	if s.reason != "run_budget" || s.flag || s.issueStatus != "blocked" || s.blockedReason != "" || s.restoreStatus != "in_progress" {
		t.Fatalf("episode = %+v, want run_budget, flag still clear, the person's block without a reason", s)
	}
	if got := len(f.suspensionComments(t)); got != 1 {
		t.Fatalf("suspension comments = %d, want 1 because the kind changed", got)
	}
}

// Design §4.4.4: in phase waiting a refusal only starts the clock; after 15
// minutes the next attempt suspends. The comment names the conflicting task.
func TestDecisionEpisodeDispatchRefusalSuspendsAfterFifteenMinutes(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	task := f.conflictingTask(t)

	f.deliverEpisodeOnly(t)
	if s := f.suspension(t); s.phase != "waiting" || s.issueStatus != "need_decision" {
		t.Fatalf("first refusal = %+v, want the episode still waiting", s)
	}
	if dbfx.Count(t, `SELECT count(*) FROM decision_wait WHERE issue_id = $1 AND closed_at IS NULL AND dispatch_refused_since IS NOT NULL`, f.issueID) != 1 {
		t.Fatal("the refusal did not start the clock")
	}

	dbfx.Exec(t, `UPDATE decision_wait SET dispatch_refused_since = now() - interval '16 minutes' WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID)
	f.deliverEpisodeOnly(t)

	s := f.suspension(t)
	if s.phase != "suspended" || s.reason != "dispatch_refused" || s.refusedTask != task || !s.flag || s.issueStatus != "blocked" || s.blockedReason != "decision-dispatch-refused" {
		t.Fatalf("episode = %+v, want suspended dispatch_refused on task %s with the episode's block", s, task)
	}
	if dbfx.Count(t, `SELECT count(*) FROM decision_wait WHERE issue_id = $1 AND closed_at IS NULL AND dispatch_refused_since IS NOT NULL`, f.issueID) != 0 {
		t.Fatal("the suspension kept the refusal clock")
	}
	comments := f.suspensionComments(t)
	if len(comments) != 1 || !strings.Contains(comments[0], task) || !strings.Contains(comments[0], "resumes when task") {
		t.Fatalf("suspension comments = %q, want one naming task %s", comments, task)
	}
	if woken, _ := f.answerState(t, answerID); woken {
		t.Fatal("a refused dispatch acknowledged the answer")
	}

	// The same kind again on a still-blocked issue: no comment, suspended_at moves.
	before := s.suspendedAt
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, task)
	next := f.conflictingTask(t)
	f.deliverEpisodeOnly(t)
	again := f.suspension(t)
	if again.phase != "suspended" || again.refusedTask != next || !again.suspendedAt.After(before) || len(f.suspensionComments(t)) != 1 {
		t.Fatalf("after the same kind again = %+v comments %d, want the new task %s, a later suspended_at and no second comment", again, len(f.suspensionComments(t)), next)
	}
}

// A suspended episode that is refused again suspends at once, and the write
// captures the person's placement as the restore target (§4.4.6).
func TestDecisionEpisodeDispatchRefusalWhileSuspendedIsImmediate(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.answerRequest(t, f.requestID, -time.Minute, false)
	f.memberMove(t, "todo").Want(200)
	task := f.conflictingTask(t)

	f.deliverEpisodeOnly(t)

	s := f.suspension(t)
	if s.reason != "dispatch_refused" || s.refusedTask != task || !s.flag || s.restoreStatus != "todo" || s.issueStatus != "blocked" {
		t.Fatalf("episode = %+v, want dispatch_refused at once with the person's todo placement saved", s)
	}
}

// Without a blocked step in the workflow the episode keeps the issue where it is
// and still suspends.
func TestDecisionEpisodeSuspensionWithoutBlockedStep(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.answerRequest(t, f.requestID, -time.Minute, false)
	f.exhaustBudget(t)
	dbfx.Exec(t, `UPDATE issue_workflow SET steps = (SELECT jsonb_agg(s) FROM jsonb_array_elements(steps) s WHERE s ->> 'status_key' <> 'blocked')
		WHERE id = (SELECT workflow_id FROM project WHERE id = (SELECT project_id FROM issue WHERE id = $1))`, f.issueID)

	f.deliverEpisodeOnly(t)
	service.WaitRunBudgetHooks()

	s := f.suspension(t)
	if s.phase != "suspended" || s.reason != "run_budget" || s.issueStatus != "need_decision" || !s.flag {
		t.Fatalf("episode = %+v, want suspended with the issue left in need_decision and the target and flag unchanged", s)
	}
}
