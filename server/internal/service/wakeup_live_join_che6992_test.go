package service

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6992 (plan CHE-6971, step 6): a condition firing of an agent-created
// wakeup joins the agent's running run as a wakeup item, behind the workspace
// setting wakeup_live_join. Every other firing runs as today.

// setLiveJoin writes workspace.settings.wakeup_live_join; "" leaves it unset.
func (e *liveEnv) setLiveJoin(t *testing.T, raw string) {
	t.Helper()
	settings := `{}`
	if raw != "" {
		settings = `{"wakeup_live_join":` + raw + `}`
	}
	e.f.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, e.f.WorkspaceID, settings)
}

func (e *liveEnv) setMode(t *testing.T, mode string) {
	t.Helper()
	e.setLiveJoin(t, `{"mode":"`+mode+`"}`)
}

// agentCreated marks the wakeup as one an agent run armed.
func (e *liveEnv) agentCreated(t *testing.T) {
	t.Helper()
	e.f.Exec(t, `UPDATE issue_wakeup SET source_task_id=$2 WHERE id=$1`, e.w.ID, e.host)
}

// joinEnv is a seeded-receipt wakeup an agent armed, with a v2 host run R
// running and the setting at block. Receipts are condition.met unless the test
// seeds others.
func joinEnv(t *testing.T) *liveEnv {
	t.Helper()
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	e.agentCreated(t)
	e.setMode(t, "block")
	return e
}

// receiptOf seeds one pending receipt of another event type.
func (e *liveEnv) receiptOf(t *testing.T, eventType string) pgtype.UUID {
	t.Helper()
	id := e.receipt(t)
	e.f.Exec(t, `UPDATE issue_wakeup_receipt SET event_type=$2 WHERE id=$1`, id, eventType)
	return id
}

// ranToday asserts the firing took today's path: one queued run of its own,
// no item, and every receipt consumed with that run.
func (e *liveEnv) ranToday(t *testing.T, got db.IssueWakeup, ids ...pgtype.UUID) {
	t.Helper()
	if n := wakeRuns(t, e.f, e.w.ID); n != 1 {
		t.Fatalf("runs = %d, want 1 (today's run)", n)
	}
	if n := e.items(t); n != 0 {
		t.Fatalf("items = %d, want 0", n)
	}
	var run string
	e.f.QueryRow(t, `SELECT id FROM agent_task_queue WHERE context->>'wakeup_id'=$1`, util.UUIDToString(e.w.ID)).Scan(&run)
	for _, id := range ids {
		var task pgtype.UUID
		var processed bool
		e.f.QueryRow(t, `SELECT task_id, processed_at IS NOT NULL FROM issue_wakeup_receipt WHERE id=$1`, id).Scan(&task, &processed)
		if !processed || util.UUIDToString(task) != run {
			t.Fatalf("receipt processed=%v task_id=%s, want consumed by today's run %s", processed, util.UUIDToString(task), run)
		}
	}
	if got.FireCount != 1 {
		t.Fatalf("fire_count = %d, want 1", got.FireCount)
	}
}

func (e *liveEnv) wouldJoin(t *testing.T) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='would_join_live'`, e.issue)
}

// condJoinEnv arms a real --until-issue style condition (status in_review)
// the way an agent would, with a v2 host run R and the setting at block.
func condJoinEnv(t *testing.T) *liveEnv {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review when ready",
		Condition: condition(t, map[string]any{"type": "issue_field", "field": "status", "value": "in_review"})})
	f.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE issue_id=$1`, issue)
	f.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE issue_id=$1`, issue)
	e := &liveEnv{f: f, s: s, issue: issue, agent: agent, w: w}
	e.host = e.run(t, agent)
	f.Exec(t, `INSERT INTO task_pr_event_capability(task_id,workspace_id,issue_id,capability) VALUES($1,$2,$3,'task-supplement-pr-event-v2')`, e.host, f.WorkspaceID, issue)
	e.agentCreated(t)
	e.setMode(t, "block")
	return e
}

func TestLiveJoinBlockDeliversConditionFiringIntoRunningRun(t *testing.T) {
	e := condJoinEnv(t)
	wakeSetStatus(t, e.f, e.issue, "in_review")
	got := e.pass(t)

	if n := wakeRuns(t, e.f, e.w.ID); n != 0 {
		t.Fatalf("a joined firing queued %d runs, want 0", n)
	}
	if n := e.items(t); n != 1 {
		t.Fatalf("items = %d, want 1", n)
	}
	var task, status, kind, note string
	var receipts int
	e.f.QueryRow(t, `SELECT task_id, status, item_kind, note, cardinality(receipt_ids) FROM task_pr_event_delivery WHERE wakeup_id=$1`, e.w.ID).Scan(&task, &status, &kind, &note, &receipts)
	if task != e.host || status != "pending" || kind != "wakeup" || receipts != 1 {
		t.Fatalf("item task=%s status=%s kind=%s receipts=%d, want host %s pending wakeup 1", task, status, kind, receipts, e.host)
	}
	for _, want := range []string{util.UUIDToString(e.w.ID), "Review when ready", "condition.met", `"status":"in_review"`, "read current state before deciding", "running"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note missing %q:\n%s", want, note)
		}
	}
	// The receipts stay pending and unreserved; a once wakeup stays on until settled.
	if n := e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL AND task_id IS NULL`, e.w.ID); n != 1 {
		t.Fatalf("pending unreserved receipts = %d, want 1", n)
	}
	if !got.Enabled || got.FireCount != 0 || got.LastTaskID.Valid {
		t.Fatalf("wakeup enabled=%v fire_count=%d last_task=%v, want on, uncounted", got.Enabled, got.FireCount, got.LastTaskID)
	}
	want := "Delivering into running run " + e.host + "; inputs retained"
	if got.LastError.String != want {
		t.Fatalf("last_error = %q, want %q", got.LastError.String, want)
	}
	if e.wouldJoin(t) != 0 {
		t.Fatal("block mode wrote would_join_live")
	}
	// The next pass holds on the open item: no run, no second item.
	again := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 0 || e.items(t) != 1 || again.FireCount != 0 {
		t.Fatalf("second pass: runs=%d items=%d fire_count=%d", n, e.items(t), again.FireCount)
	}
}

func TestLiveJoinSettlesAsJoinedAfterAckAndHostCompletes(t *testing.T) {
	e := condJoinEnv(t)
	wakeSetStatus(t, e.f, e.issue, "in_review")
	e.pass(t)
	e.f.Exec(t, `UPDATE task_pr_event_delivery SET status='delivered', attempt_count=1 WHERE wakeup_id=$1`, e.w.ID)
	e.endHost(t, "completed")
	got := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 0 {
		t.Fatalf("a joined firing started %d runs", n)
	}
	if got.FireCount != 1 || got.Enabled || util.UUIDToString(got.LastTaskID) != e.host {
		t.Fatalf("fire_count=%d enabled=%v last_task=%s, want 1, off, host", got.FireCount, got.Enabled, util.UUIDToString(got.LastTaskID))
	}
	if n := e.triggered(t, "joined_live"); n != 1 {
		t.Fatalf("joined_live rows = %d, want 1", n)
	}
}

func TestLiveJoinWarnRunsAsTodayAndNamesHost(t *testing.T) {
	for name, raw := range map[string]string{"warn": `{"mode":"warn"}`, "unset": ""} {
		t.Run(name, func(t *testing.T) {
			e := joinEnv(t)
			e.setLiveJoin(t, raw)
			r := e.receipt(t)
			e.ranToday(t, e.pass(t), r)
			if n := e.wouldJoin(t); n != 1 {
				t.Fatalf("would_join_live rows = %d, want 1", n)
			}
			var host, wakeup string
			e.f.QueryRow(t, `SELECT details->>'task_id', details->>'wakeup_id' FROM activity_log WHERE issue_id=$1 AND action='would_join_live'`, e.issue).Scan(&host, &wakeup)
			if host != e.host || wakeup != util.UUIDToString(e.w.ID) {
				t.Fatalf("would_join_live task_id=%q wakeup_id=%q, want host %s", host, wakeup, e.host)
			}
		})
	}
}

func TestLiveJoinOffOrUnreadableRunsAsToday(t *testing.T) {
	for name, raw := range map[string]string{
		"off":          `{"mode":"off"}`,
		"unknown_mode": `{"mode":"always"}`,
		"unreadable":   `"block"`,
	} {
		t.Run(name, func(t *testing.T) {
			e := joinEnv(t)
			e.setLiveJoin(t, raw)
			r := e.receipt(t)
			e.ranToday(t, e.pass(t), r)
			if e.wouldJoin(t) != 0 {
				t.Fatal("wrote would_join_live")
			}
		})
	}
}

func TestLiveJoinOnlyConditionReceiptsJoin(t *testing.T) {
	for _, eventType := range []string{"time.due", "wakeup.timeout", "wakeup.manual", "comment.created", "reaction.added", "task.completed"} {
		t.Run(eventType, func(t *testing.T) {
			e := joinEnv(t)
			r := e.receiptOf(t, eventType)
			e.ranToday(t, e.pass(t), r)
		})
	}
	t.Run("mixed", func(t *testing.T) {
		e := joinEnv(t)
		a, b := e.receipt(t), e.receiptOf(t, "comment.created")
		e.ranToday(t, e.pass(t), a, b)
	})
}

func TestLiveJoinRuleEligibility(t *testing.T) {
	cases := map[string]func(e *liveEnv, t *testing.T){
		"member_created": func(e *liveEnv, t *testing.T) {
			e.f.Exec(t, `UPDATE issue_wakeup SET source_task_id=NULL WHERE id=$1`, e.w.ID)
		},
		"decision_bound": func(e *liveEnv, t *testing.T) {
			e.f.Exec(t, `UPDATE issue_wakeup SET decision_request_id=gen_random_uuid() WHERE id=$1`, e.w.ID)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := joinEnv(t)
			mutate(e, t)
			r := e.receipt(t)
			e.ranToday(t, e.pass(t), r)
		})
	}
}

func TestLiveJoinRuleEligibilityPredicate(t *testing.T) {
	agent := db.IssueWakeup{SourceTaskID: pgtype.UUID{Valid: true}}
	if !liveJoinRuleEligible(agent, nil) {
		t.Fatal("an agent-created plain wakeup must be eligible")
	}
	for name, w := range map[string]db.IssueWakeup{
		"member":   {},
		"decision": {SourceTaskID: agent.SourceTaskID, DecisionRequestID: pgtype.UUID{Valid: true}},
		"system":   {SourceTaskID: agent.SourceTaskID, SystemRule: pgtype.Text{String: "x", Valid: true}},
		"default":  {SourceTaskID: agent.SourceTaskID, DefaultRuleKey: pgtype.Text{String: "x", Valid: true}},
		"paused":   {SourceTaskID: agent.SourceTaskID, PausedReason: pgtype.Text{String: "loop", Valid: true}},
	} {
		if liveJoinRuleEligible(w, nil) {
			t.Fatalf("%s wakeup must not join", name)
		}
	}
	if liveJoinRuleEligible(agent, &builtinWakeup{}) {
		t.Fatal("a custom-rule instance must not join")
	}
}

func TestLiveJoinRunShape(t *testing.T) {
	t.Run("another_runAs", func(t *testing.T) {
		e := joinEnv(t)
		other := e.f.member(t, "other")
		e.f.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2, accountable_user_id=$2 WHERE id=$1`, e.host, other)
		r := e.receipt(t)
		e.ranToday(t, e.pass(t), r)
	})
	t.Run("v1_task", func(t *testing.T) {
		e := joinEnv(t)
		e.f.Exec(t, `UPDATE task_pr_event_capability SET capability='task-supplement-pr-event-v1' WHERE task_id=$1`, e.host)
		r := e.receipt(t)
		e.ranToday(t, e.pass(t), r)
	})
	t.Run("no_capability", func(t *testing.T) {
		e := joinEnv(t)
		e.f.Exec(t, `DELETE FROM task_pr_event_capability WHERE task_id=$1`, e.host)
		r := e.receipt(t)
		e.ranToday(t, e.pass(t), r)
	})
	t.Run("host_not_running", func(t *testing.T) {
		e := joinEnv(t)
		e.endHost(t, "completed")
		r := e.receipt(t)
		e.ranToday(t, e.pass(t), r)
	})
	t.Run("two_running_runs", func(t *testing.T) {
		e := joinEnv(t)
		second := e.run(t, e.agent)
		e.f.Exec(t, `INSERT INTO task_pr_event_capability(task_id,workspace_id,issue_id,capability) VALUES($1,$2,$3,'task-supplement-pr-event-v2')`, second, e.f.WorkspaceID, e.issue)
		r := e.receipt(t)
		e.ranToday(t, e.pass(t), r)
	})
}

func TestLiveJoinWaitingRunKeepsItsOwnJoin(t *testing.T) {
	e := joinEnv(t)
	e.f.Task(t, e.agent, testutil.Cols{"issue_id": e.issue, "status": "queued",
		"runtime_id":         testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + e.agent + "')"),
		"originator_user_id": e.f.UserID, "accountable_user_id": e.f.UserID})
	r := e.receipt(t)
	got := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 0 {
		t.Fatalf("runs = %d, want 0 (the waiting run takes the inputs)", n)
	}
	if n := e.items(t); n != 0 {
		t.Fatalf("items = %d, want 0: the waiting-run join keeps priority", n)
	}
	if !e.pending(t, r) || !strings.Contains(got.LastError.String, "Waiting for a queued run") {
		t.Fatalf("receipt pending=%v last_error=%q", e.pending(t, r), got.LastError.String)
	}
}

func TestLiveJoinReceiptAlreadyInAnItemRunsToday(t *testing.T) {
	e := joinEnv(t)
	r := e.receipt(t)
	e.enqueue(t, r)
	e.setItem(t, "failed")
	e.f.Exec(t, `UPDATE task_pr_event_delivery SET status='fallback' WHERE event_id=$1`, e.item.EventID)
	e.f.Exec(t, `UPDATE task_pr_event_delivery SET failure_reason='rejected' WHERE event_id=$1`, e.item.EventID)
	got := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 1 {
		t.Fatalf("runs = %d, want 1: a receipt never joins twice", n)
	}
	if n := e.items(t); n != 1 {
		t.Fatalf("items = %d, want 1 (the fallback item only)", n)
	}
	if got.FireCount != 1 {
		t.Fatalf("fire_count = %d, want 1", got.FireCount)
	}
}

func TestLiveJoinPausedAgentSkipConsumesAndDoesNotJoin(t *testing.T) {
	e := joinEnv(t)
	e.f.Exec(t, `UPDATE agent SET scheduling_paused_at=now() WHERE id=$1`, e.agent)
	r := e.receipt(t)
	e.pass(t)
	if n := e.items(t); n != 0 || wakeRuns(t, e.f, e.w.ID) != 0 {
		t.Fatalf("items=%d runs=%d, want none", n, wakeRuns(t, e.f, e.w.ID))
	}
	if e.pending(t, r) {
		t.Fatal("the paused-agent skip must consume the firing")
	}
}

// CHE-6968 stays the one verdict on a time fire: a deferred unchanged time
// fire is no condition firing, so the join never sees it, even with a v2 run
// running and wakeup_live_join at block.
func TestLiveJoinLeavesDeferredTimeFireToCHE6968(t *testing.T) {
	e := newSkipEnv(t, false)
	e.mode(t, `{"time_wakeup_skip":{"mode":"block"},"wakeup_live_join":{"mode":"block"}}`)
	id := e.declared(t, "issue")
	wakeTick(t, e.f, e.s, id)
	e.finishRuns(t, "completed")
	live := &liveEnv{f: e.f, s: e.s, issue: e.issue, agent: e.agent}
	host := live.run(t, e.agent)
	e.f.Exec(t, `INSERT INTO task_pr_event_capability(task_id,workspace_id,issue_id,capability) VALUES($1,$2,$3,'task-supplement-pr-event-v2')`, host, e.f.WorkspaceID, e.issue)
	e.f.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE issue_id=$1`, e.issue)
	e.f.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE issue_id=$1`, e.issue)
	wakeTick(t, e.f, e.s, id)
	if n := e.f.Count(t, `SELECT count(*) FROM task_pr_event_delivery WHERE issue_id=$1`, e.issue); n != 0 {
		t.Fatalf("a deferred time fire created %d items, want 0", n)
	}
	if got := e.acts(t, "wake_deferred"); got != 1 {
		t.Fatalf("wake_deferred rows = %d, want 1: CHE-6968 owns the verdict", got)
	}
	if got := wakeRuns(t, e.f, id); got != 1 {
		t.Fatalf("runs = %d, want 1 (the first fire only)", got)
	}
	if n := e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='would_join_live'`, e.issue); n != 0 {
		t.Fatalf("would_join_live rows = %d, want 0", n)
	}
}
