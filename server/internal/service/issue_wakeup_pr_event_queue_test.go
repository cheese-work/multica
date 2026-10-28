package service

import (
	"context"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Next-turn fallback and switch-off drain (CHE-2298 a3).

// A daemon without the PR-event channel never takes a mid-run notice: every
// fact that arrives while its run is live waits, and one wake carries them all.
func TestLegacyDaemonPRFactsStartOneNextTurnWakeWithAllCauses(t *testing.T) {
	e := newBuiltinEnv(t)
	run := e.runningRun(t, e.f.UserID)

	e.mergedPR(t, 801)
	e.mergedPR(t, 802)
	e.failedChecks(t, "head-1", "FAILURE")
	if e.tasks(t, SystemRulePRMerged)+e.tasks(t, SystemRulePRChecksFailed) != 0 {
		t.Fatal("a PR fact started a second run while the first was live")
	}
	if e.pending(t, SystemRulePRMerged) != 2 || e.pending(t, SystemRulePRChecksFailed) != 1 {
		t.Fatalf("facts were dropped: merged pending %d (want 2), checks pending %d (want 1)", e.pending(t, SystemRulePRMerged), e.pending(t, SystemRulePRChecksFailed))
	}
	if e.outcomeCount(t, SystemRulePRMerged, "queued_next_turn") != 2 {
		t.Fatalf("queueing is not visible per cause: %v", e.outcomes(t, SystemRulePRMerged))
	}
	var message string
	e.f.QueryRow(t, `SELECT details->>'message' FROM activity_log WHERE issue_id=$1 AND details->>'outcome'='queued_next_turn' ORDER BY created_at,id LIMIT 1`, e.issue).Scan(&message)
	if !strings.HasPrefix(message, "PR event queued for next turn: ") || !strings.Contains(message, "#801") {
		t.Fatalf("activity text = %q", message)
	}

	e.endRun(t, run)
	e.tick(t, SystemRulePRMerged)
	e.tick(t, SystemRulePRChecksFailed)
	if got := e.tasks(t, SystemRulePRMerged) + e.tasks(t, SystemRulePRChecksFailed); got != 1 {
		t.Fatalf("next turn started %d runs, want 1", got)
	}
	note := e.lastTask(t, SystemRulePRMerged).HandoffNote.String
	for _, number := range []int32{801, 802} {
		if !strings.Contains(note, e.prNote(t, number)) {
			t.Fatalf("the next-turn wake lacks PR %d: %q", number, note)
		}
	}
}

// prEventEnv is a running run holding the PR-event capability, plus one event
// the run is delivering when it ends.
type prEventEnv struct {
	builtinEnv
	run, event string
}

func newPREventEnv(t *testing.T) prEventEnv {
	t.Helper()
	e := newBuiltinEnv(t)
	run := e.runningRun(t, e.f.UserID)
	ws := e.f.WorkspaceID
	var prID string
	e.f.QueryRow(t, `INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1, 1, 'acme', 'widget', 903, 'a title', 'open', 'https://example.test/pr', now(), now(), 'new-head') RETURNING id`, ws).Scan(&prID)
	e.f.Cleanup(t, `DELETE FROM github_pull_request WHERE id=$1`, prID)
	e.f.Cleanup(t, `DELETE FROM issue_pr_event WHERE issue_id=$1`, e.issue)
	e.f.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE task_id=$1`, run)
	e.f.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id=$1`, run)
	e.f.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, prID)
	e.f.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, e.issue, prID)
	e.f.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, 'task-supplement-pr-event-v1')`, run, ws, e.issue)
	var event string
	e.f.QueryRow(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key)
		VALUES ($1, $2, $3, 'head_changed', 'new-head', 'base-1', now(), 'head_changed|new-head') RETURNING id`, ws, e.issue, prID).Scan(&event)
	return prEventEnv{builtinEnv: e, run: run, event: event}
}

func (e prEventEnv) status(t *testing.T) string {
	t.Helper()
	var s string
	e.f.QueryRow(t, `SELECT status FROM issue_pr_event WHERE id=$1`, e.event).Scan(&s)
	return s
}

// delivering hands the event to the live run; ending the run settles it the way
// every terminal transition does.
func (e prEventEnv) endWhileDelivering(t *testing.T) {
	t.Helper()
	rows, err := e.f.q.EnqueueTaskPREventDelivery(context.Background(), db.EnqueueTaskPREventDeliveryParams{TaskID: parseTestUUID(t, e.run), EventID: parseTestUUID(t, e.event)})
	if err != nil || !rows.TaskID.Valid {
		t.Fatalf("setup: enqueue delivery: %v", err)
	}
	e.endRun(t, e.run)
	task, err := e.f.q.GetAgentTask(context.Background(), parseTestUUID(t, e.run))
	if err != nil {
		t.Fatal(err)
	}
	if err := SettleTerminalTaskState(context.Background(), e.f.q, task); err != nil {
		t.Fatal(err)
	}
	if got := e.status(t); got != "next_turn" {
		t.Fatalf("setup: event is %q after turn end, want next_turn", got)
	}
}

func (e prEventEnv) tickAll(t *testing.T, s *IssueWakeupService) {
	t.Helper()
	if err := s.TickWorkspaces(context.Background(), parseTestUUID(t, e.f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
}

func (e prEventEnv) attentionNote(t *testing.T) string {
	t.Helper()
	return e.lastTask(t, SystemRulePRAttention).HandoffNote.String
}

// A delivery that loses the race with turn end moves its cause to the next turn.
func TestPREventLosingTheRaceWithTurnEndWakesTheNextTurn(t *testing.T) {
	e := newPREventEnv(t)
	e.endWhileDelivering(t)

	e.tickAll(t, e.s)

	if e.tasks(t, SystemRulePRAttention) != 1 {
		t.Fatalf("next turn started %d runs, want 1", e.tasks(t, SystemRulePRAttention))
	}
	if note := e.attentionNote(t); !strings.Contains(note, "head_changed") || !strings.Contains(note, "903") {
		t.Fatalf("the wake does not list the cause: %q", note)
	}
	if got := e.status(t); got != "delivered" {
		t.Fatalf("event is %q after the wake started, want delivered", got)
	}
	e.tickAll(t, e.s)
	if e.tasks(t, SystemRulePRAttention) != 1 {
		t.Fatal("a later pass woke the same cause again")
	}
}

// Switching an event kind off stops capture only. A cause already accepted
// still drains after a restart, and stays stored and visible when its rule is off.
func TestPREventSwitchOffKeepsPendingCauses(t *testing.T) {
	off := func(t *testing.T, e prEventEnv, settings string) {
		t.Helper()
		e.f.Exec(t, `UPDATE workspace SET settings = settings || $2::jsonb WHERE id=$1`, e.f.WorkspaceID, settings)
	}
	t.Run("capture switch off, then restart", func(t *testing.T) {
		e := newPREventEnv(t)
		e.endWhileDelivering(t)
		off(t, e, `{"pr_event_capture":{"head_changed":false}}`)
		restarted := &IssueWakeupService{Tasks: e.s.Tasks}

		e.tickAll(t, restarted)

		if e.tasks(t, SystemRulePRAttention) != 1 || !strings.Contains(e.attentionNote(t), "head_changed") {
			t.Fatalf("the pending cause was not delivered after the switch went off and the service restarted")
		}
	})
	t.Run("rule switched off keeps the cause visible", func(t *testing.T) {
		e := newPREventEnv(t)
		e.endWhileDelivering(t)
		off(t, e, `{"`+WorkspaceSettingPRAttention+`":false}`)

		e.tickAll(t, &IssueWakeupService{Tasks: e.s.Tasks})

		if e.tasks(t, SystemRulePRAttention) != 0 {
			t.Fatal("a switched-off rule started a run")
		}
		if got := e.status(t); got != "queued" {
			t.Fatalf("event is %q, want it stored and visible as queued", got)
		}
	})
}
