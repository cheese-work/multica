package service

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
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
	t.Run("rule switched off keeps the cause visible and recoverable", func(t *testing.T) {
		e := newPREventEnv(t)
		e.endWhileDelivering(t)
		off(t, e, `{"`+WorkspaceSettingPRAttention+`":false}`)

		e.tickAll(t, &IssueWakeupService{Tasks: e.s.Tasks})

		if e.tasks(t, SystemRulePRAttention) != 0 {
			t.Fatal("a switched-off rule started a run")
		}
		if got := e.status(t); got != "next_turn" {
			t.Fatalf("event is %q, want it stored as next_turn", got)
		}
		if e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND details->>'message' LIKE 'PR event queued for next turn:%'`, e.issue) != 1 {
			t.Fatal("the pending cause is not visible in the activity")
		}
		e.tickAll(t, &IssueWakeupService{Tasks: e.s.Tasks})
		if e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND details->>'message' LIKE 'PR event queued for next turn:%'`, e.issue) != 1 {
			t.Fatal("a later pass noted the same cause again")
		}

		off(t, e, `{"`+WorkspaceSettingPRAttention+`":true}`)
		e.tickAll(t, &IssueWakeupService{Tasks: e.s.Tasks})
		if e.tasks(t, SystemRulePRAttention) != 1 || !strings.Contains(e.attentionNote(t), "head_changed") {
			t.Fatal("re-enabling the rule did not deliver the cause")
		}
	})
	t.Run("rule switched off after the cause was pending", func(t *testing.T) {
		e := newPREventEnv(t)
		e.endWhileDelivering(t)
		// Bridge without dispatching, so the receipt is still pending when the rule goes off.
		if err := e.s.queueNextTurnPREvents(context.Background(), []pgtype.UUID{parseTestUUID(t, e.f.WorkspaceID)}); err != nil {
			t.Fatal(err)
		}
		if e.pending(t, SystemRulePRAttention) != 1 {
			t.Fatalf("setup: pending = %d, want 1", e.pending(t, SystemRulePRAttention))
		}
		off(t, e, `{"`+WorkspaceSettingPRAttention+`":false}`)
		e.tickAll(t, &IssueWakeupService{Tasks: e.s.Tasks})
		e.tickAll(t, &IssueWakeupService{Tasks: e.s.Tasks})
		if got := e.status(t); got != "next_turn" {
			t.Fatalf("event is %q after the rule went off, want next_turn", got)
		}
		off(t, e, `{"`+WorkspaceSettingPRAttention+`":true}`)
		e.tickAll(t, &IssueWakeupService{Tasks: e.s.Tasks})
		if e.tasks(t, SystemRulePRAttention) != 1 {
			t.Fatal("re-enabling the rule did not deliver the cause the switch-off had discarded")
		}
	})
}

// An accepted event with no live run takes the next-turn path without anyone
// enqueueing a delivery.
func TestPREventWithoutLiveRunWakesNextTurn(t *testing.T) {
	e := newPREventEnv(t)
	e.endRun(t, e.run)

	e.tickAll(t, e.s)

	if e.tasks(t, SystemRulePRAttention) != 1 || !strings.Contains(e.attentionNote(t), "head_changed") {
		t.Fatalf("an accepted event with no live run started %d runs, event is %q", e.tasks(t, SystemRulePRAttention), e.status(t))
	}
}

// A project-level pr_needs_attention definition still receives the cause.
func TestPREventReachesAScopedAttentionRule(t *testing.T) {
	e := newPREventEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRAttention, `"instruction":"Review every PR cause"`)
	e.endWhileDelivering(t)

	e.tickAll(t, e.s)

	if e.tasks(t, SystemRulePRAttention) != 1 || !strings.Contains(e.attentionNote(t), "head_changed") {
		t.Fatalf("scoped rule started %d runs, event is %q, pending %d", e.tasks(t, SystemRulePRAttention), e.status(t), e.pending(t, SystemRulePRAttention))
	}
}

// The cause is delivered when its wake starts, not when it is queued: a wake
// cancelled before it started hands the cause back.
func TestPREventStaysUntilItsWakeStarts(t *testing.T) {
	e := newPREventEnv(t)
	e.endWhileDelivering(t)
	e.tickAll(t, e.s)
	wake := e.lastTask(t, SystemRulePRAttention)
	if got := e.status(t); got != "next_turn" {
		t.Fatalf("event is %q while its wake has not started, want next_turn", got)
	}

	e.f.Exec(t, `UPDATE agent_task_queue SET status='cancelled', completed_at=now() WHERE id=$1`, wake.ID)
	cancelled, err := e.f.q.GetAgentTask(context.Background(), wake.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := SettleTerminalTaskState(context.Background(), e.f.q, cancelled); err != nil {
		t.Fatal(err)
	}
	e.tickAll(t, e.s)
	if e.tasks(t, SystemRulePRAttention) != 2 {
		t.Fatalf("the cause was lost with the cancelled wake: %d wakes", e.tasks(t, SystemRulePRAttention))
	}

	next := e.lastTask(t, SystemRulePRAttention)
	e.f.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, next.ID)
	e.tickAll(t, e.s)
	if got := e.status(t); got != "delivered" {
		t.Fatalf("event is %q after its wake started, want delivered", got)
	}
}

// beforeBeginTx runs one hook just before the next transaction opens: the point
// between the bridge's settlement and its selection.
type beforeBeginTx struct {
	TxStarter
	before func()
}

func (b *beforeBeginTx) Begin(ctx context.Context) (pgx.Tx, error) {
	if b.before != nil {
		before := b.before
		b.before = nil
		before()
	}
	return b.TxStarter.Begin(ctx)
}

// A carrier that starts between settlement and selection already carries the
// cause: it must not become eligible for a second wake.
func TestPREventCarrierStartingMidPassIsNotWokenTwice(t *testing.T) {
	e := newPREventEnv(t)
	e.endWhileDelivering(t)
	e.tickAll(t, e.s)
	wake := e.lastTask(t, SystemRulePRAttention)
	original := e.s.Tasks.TxStarter
	e.s.Tasks.TxStarter = &beforeBeginTx{TxStarter: original, before: func() {
		e.f.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, wake.ID)
	}}
	t.Cleanup(func() { e.s.Tasks.TxStarter = original })
	e.tickAll(t, e.s)
	e.s.Tasks.TxStarter = original

	e.endRun(t, util.UUIDToString(wake.ID))
	e.tickAll(t, &IssueWakeupService{Tasks: e.s.Tasks})
	if got := e.tasks(t, SystemRulePRAttention); got != 1 {
		t.Fatalf("one event produced %d wakes after its carrier started, want 1", got)
	}
}

// A full batch of causes the rule cannot take now must not keep later events
// out of every pass.
func TestPREventHeldBatchDoesNotStarveOtherIssues(t *testing.T) {
	held := newPREventEnv(t)
	held.endWhileDelivering(t)
	held.f.Exec(t, `UPDATE workspace SET settings=settings || $2::jsonb WHERE id=$1`, held.f.WorkspaceID, `{"`+WorkspaceSettingPRAttention+`":false}`)
	held.f.Exec(t, `UPDATE issue_pr_event SET observed_at=now()-interval '1 hour' WHERE id=$1`, held.event)
	held.f.Exec(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status)
		SELECT workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key || ':batch:' || sequence, 'next_turn'
		FROM issue_pr_event CROSS JOIN generate_series(1,99) AS sequence WHERE id=$1`, held.event)
	active := newPREventEnv(t)
	active.endRun(t, active.run)

	for pass := 0; pass < 3; pass++ {
		if err := active.s.TickWorkspaces(context.Background(), parseTestUUID(t, held.f.WorkspaceID), parseTestUUID(t, active.f.WorkspaceID)); err != nil {
			t.Fatal(err)
		}
	}
	if got := active.tasks(t, SystemRulePRAttention); got != 1 {
		t.Fatalf("100 held causes starved another issue for 3 passes: %d wakes, event is %q", got, active.status(t))
	}
}
