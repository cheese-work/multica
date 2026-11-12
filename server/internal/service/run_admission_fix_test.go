package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Fix round for the CHE-5213 review: resume atomicity, non-issue project runs,
// orphaned inputs, comment coalescing.

func (e *admEnv) quickCreate() pgtype.UUID {
	e.t.Helper()
	task, err := e.svc.EnqueueQuickCreateTask(context.Background(), util.MustParseUUID(e.ws), util.MustParseUUID(e.user),
		util.MustParseUUID(e.agent), pgtype.UUID{}, "make an issue", "", "", util.MustParseUUID(e.project), pgtype.UUID{}, nil)
	if err != nil {
		e.t.Fatalf("quick create: %v", err)
	}
	return task.ID
}

func (e *admEnv) autopilotRunOnly() pgtype.UUID {
	e.t.Helper()
	ctx := context.Background()
	var ap, run, task pgtype.UUID
	if err := e.pool.QueryRow(ctx, `INSERT INTO autopilot (workspace_id, title, assignee_id, execution_mode, created_by_type, created_by_id, project_id)
		VALUES ($1, 'ap', $2, 'run_only', 'member', $3, $4) RETURNING id`, e.ws, e.agent, e.user, e.project).Scan(&ap); err != nil {
		e.t.Fatalf("seed autopilot: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO autopilot_run (autopilot_id, source, status) VALUES ($1, 'manual', 'running') RETURNING id`, ap).Scan(&run); err != nil {
		e.t.Fatalf("seed autopilot run: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, autopilot_run_id) VALUES ($1, $2, 'queued', $3) RETURNING id`,
		e.agent, e.runtime, run).Scan(&task); err != nil {
		e.t.Fatalf("seed autopilot task: %v", err)
	}
	return task
}

func TestAdmissionQuickCreateInPausedProjectIsHeldThenStartsOnce(t *testing.T) {
	e := newAdmEnv(t)
	e.setStatus("paused")
	id := e.quickCreate()
	if got := e.claim(); got != nil {
		t.Fatalf("quick-create run started in a paused project: %v", got.ID)
	}
	if w := e.where(id); w != "queued/held" {
		t.Fatalf("quick-create run = %s, want queued/held", w)
	}
	e.setStatus("in_progress")
	if got := e.claim(); got == nil || got.ID != id {
		t.Fatalf("claim after unpause = %v, want %v", got, id)
	}
}

func TestAdmissionAutopilotRunOnlyCountsAgainstWIP(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	ap := e.autopilotRunOnly()
	first := e.claim()
	if first == nil || first.ID != ap {
		t.Fatalf("autopilot run did not start: %v", first)
	}
	e.enqueue(e.issue("medium"))
	if got := e.claim(); got != nil {
		t.Fatalf("issue run started past WIP 1 with an autopilot run in flight: %v", got.ID)
	}
	e.complete(first)
	if got := e.claim(); got == nil {
		t.Fatal("issue run did not start after the autopilot run ended")
	}
}

func TestAdmissionQuickCreateWaitsForWIPSlotAndReleaseReachesIt(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	e.enqueue(e.issue("medium"))
	running := e.claim()
	qc := e.quickCreate()
	if got := e.claim(); got != nil {
		t.Fatalf("quick-create ran past WIP 1: %v", got.ID)
	}
	e.complete(running)
	if w := e.where(qc); w != "queued/reserved" {
		t.Fatalf("quick-create after the slot freed = %s, want queued/reserved", w)
	}
}

func TestAdmissionResumeWithChangedAssigneeKeepsInputsParked(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	e.fail(e.claim(), "agent_error")
	if _, err := e.pool.Exec(context.Background(), `UPDATE issue SET assignee_id = $2 WHERE id = $1`, issue, e.agent2); err != nil {
		t.Fatal(err)
	}
	n, _ := e.svc.ResumeParkedInputs(context.Background(), util.MustParseUUID(issue), util.MustParseUUID(e.agent))
	if n != 0 {
		t.Fatalf("resume re-held %d inputs for an agent that is no longer the assignee", n)
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("inputs = %v, want [parked]", got)
	}
}

func TestAdmissionResumeRollsBackWhenCarrierRunFails(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	e.fail(e.claim(), "agent_error")
	e.setStatus("cancelled") // the carrier run is refused in an inactive project
	if _, err := e.svc.ResumeParkedInputs(context.Background(), util.MustParseUUID(issue), util.MustParseUUID(e.agent)); err == nil {
		t.Fatal("resume succeeded although no run could be queued")
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("inputs = %v, want [parked] after the failed resume", got)
	}
}

func TestAdmissionHeldInputWithoutRunIsParkedBySweep(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.record(issue, e.agent, "orphan-1", "trigger")
	if err := e.svc.ReconcileAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"parked"}) {
		t.Fatalf("orphaned input = %v, want [parked]", got)
	}
}

func TestAdmissionHeldInputWithQueuedRunIsNotSwept(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	if err := e.svc.ReconcileAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.inputStates(issue); !eqStrings(got, []string{"held"}) {
		t.Fatalf("input with a queued run = %v, want [held]", got)
	}
}

func TestAdmissionCoalescedCommentGetsOwnInput(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	task := e.enqueue(issue)
	var cid string
	if err := e.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()`).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_task_queue SET coalesced_comment_ids = ARRAY[$2::uuid] WHERE id = $1`, task.ID, cid); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_input WHERE issue_id = $1 AND event_id = $2`, issue, "comment:"+cid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("coalesced comment inputs = %d (err %v), want 1", n, err)
	}
}

func TestAdmissionTriggerEventIDIsStableAcrossRedelivery(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	e.enqueue(issue)
	var kind string
	var payload []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT kind, payload FROM run_input WHERE issue_id = $1`, issue).Scan(&kind, &payload); err != nil {
		t.Fatal(err)
	}
	if kind != "trigger" || len(payload) < 3 {
		t.Fatalf("kind=%q payload=%s", kind, payload)
	}
}

func TestAdmissionTriggerWithEventIDKeepsItAcrossRedelivery(t *testing.T) {
	e := newAdmEnv(t)
	issue := e.issue("medium")
	ctx := WithEventID(context.Background(), "evt-42")
	iss, _ := e.svc.Queries.GetIssue(ctx, util.MustParseUUID(issue))
	if _, err := e.svc.EnqueueTaskForIssue(ctx, iss); err != nil {
		t.Fatal(err)
	}
	var id, payload string
	if err := e.pool.QueryRow(context.Background(), `SELECT event_id, payload::text FROM run_input WHERE issue_id = $1`, issue).Scan(&id, &payload); err != nil {
		t.Fatal(err)
	}
	if id != "event:evt-42" {
		t.Fatalf("event id = %q, want event:evt-42", id)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "trigger" || got["event_id"] != "evt-42" {
		t.Fatalf("payload = %s", payload)
	}
	for _, key := range []string{"trigger_summary", "originator_source", "handoff_note"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("payload lacks %s: %s", key, payload)
		}
	}
}

func TestPurgeWriteEventsDropsOnlyOldRows(t *testing.T) {
	e := newAdmEnv(t)
	ctx := context.Background()
	t.Cleanup(func() { e.pool.Exec(ctx, `DELETE FROM write_event WHERE workspace_id = $1`, e.ws) })
	for _, row := range []struct{ key, age string }{{"old", "30 days"}, {"fresh", "1 minute"}} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO write_event (workspace_id, event_key, owner, state, created_at) VALUES ($1, $2, gen_random_uuid(), 'done', now() - $3::interval)`, e.ws, row.key, row.age); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.svc.PurgeWriteEvents(ctx); err != nil || n < 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	var keys []string
	rows, _ := e.pool.Query(ctx, `SELECT event_key FROM write_event WHERE workspace_id = $1`, e.ws)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 1 || keys[0] != "fresh" {
		t.Fatalf("write events after purge = %v, want [fresh]", keys)
	}
}

func (e *admEnv) chatRun() pgtype.UUID {
	e.t.Helper()
	ctx := context.Background()
	var session, task pgtype.UUID
	if err := e.pool.QueryRow(ctx, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, project_id)
		VALUES ($1, $2, $3, 'chat', $4) RETURNING id`, e.ws, e.agent, e.user, e.project).Scan(&session); err != nil {
		e.t.Fatalf("seed chat session: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, chat_session_id) VALUES ($1, $2, 'queued', $3) RETURNING id`,
		e.agent, e.runtime, session).Scan(&task); err != nil {
		e.t.Fatalf("seed chat task: %v", err)
	}
	return task
}

func TestAdmissionProjectChatRunIsHeldInPausedProject(t *testing.T) {
	e := newAdmEnv(t)
	e.setStatus("paused")
	id := e.chatRun()
	if got := e.claim(); got != nil {
		t.Fatalf("chat run started in a paused project: %v", got.ID)
	}
	if w := e.where(id); w != "queued/held" {
		t.Fatalf("chat run = %s, want queued/held", w)
	}
	e.setStatus("in_progress")
	if got := e.claim(); got == nil || got.ID != id {
		t.Fatalf("claim after unpause = %v, want %v", got, id)
	}
}

func TestAdmissionChatRunCountsAgainstWIP(t *testing.T) {
	e := newAdmEnv(t)
	e.setWIP(1)
	chat := e.chatRun()
	first := e.claim()
	if first == nil || first.ID != chat {
		t.Fatalf("chat run did not start: %v", first)
	}
	e.enqueue(e.issue("medium"))
	if got := e.claim(); got != nil {
		t.Fatalf("issue run started past WIP 1 with a chat run in flight: %v", got.ID)
	}
	e.complete(first)
	if got := e.claim(); got == nil {
		t.Fatal("issue run did not start after the chat run ended")
	}
}

// Round 2, P1-1: a trigger a queued run absorbs is stored as an input with the
// producer's event id, through each real enqueue producer.

func (e *admEnv) eventIDs(issueID string) map[string]int {
	e.t.Helper()
	out := map[string]int{}
	for _, in := range e.inputs(issueID) {
		out[in.EventID]++
	}
	return out
}

func (e *admEnv) issueRow(issueID string) db.Issue {
	e.t.Helper()
	issue, err := e.svc.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		e.t.Fatalf("get issue: %v", err)
	}
	return issue
}

func wantCoalescedRecorded(t *testing.T, e *admEnv, issueID string, produce func(event string) error) {
	t.Helper()
	for _, ev := range []string{"evt-A", "evt-A", "evt-B"} {
		if err := produce(ev); !errors.Is(err, ErrDuplicatePendingTask) {
			t.Fatalf("producer(%s) = %v, want ErrDuplicatePendingTask", ev, err)
		}
	}
	got := e.eventIDs(issueID)
	if got["event:evt-A"] != 1 || got["event:evt-B"] != 1 {
		t.Fatalf("inputs = %v, want event:evt-A and event:evt-B once each", got)
	}
	run := e.claim()
	if run == nil {
		t.Fatal("no run to claim")
	}
	batch, _ := e.svc.RunInputsForTask(context.Background(), run.ID)
	in := map[string]bool{}
	for _, b := range batch {
		in[b.EventID] = true
	}
	if !in["event:evt-A"] || !in["event:evt-B"] {
		t.Fatalf("claimed batch = %v, want both absorbed triggers", in)
	}
}

func TestAdmissionCoalescedIssueEnqueueIsRecordedWithProducerEvent(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	e.enqueue(issueID)
	issue := e.issueRow(issueID)
	wantCoalescedRecorded(t, e, issueID, func(ev string) error {
		_, err := e.svc.EnqueueTaskForIssue(WithEventID(context.Background(), ev), issue)
		return err
	})
}

func TestAdmissionCoalescedMentionEnqueueIsRecordedWithProducerEvent(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	e.enqueue(issueID)
	issue := e.issueRow(issueID)
	wantCoalescedRecorded(t, e, issueID, func(ev string) error {
		_, err := e.svc.EnqueueTaskForMention(WithEventID(context.Background(), ev), issue, util.MustParseUUID(e.agent), pgtype.UUID{}, OriginDerived)
		return err
	})
}

func TestAdmissionCoalescedTxEnqueueIsRecordedWithProducerEvent(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	e.enqueue(issueID)
	issue := e.issueRow(issueID)
	wantCoalescedRecorded(t, e, issueID, func(ev string) error {
		tx, err := e.pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(context.Background())
		_, _, err = e.svc.EnqueueIssueRunInTx(WithEventID(context.Background(), ev), e.svc.Queries.WithTx(tx), issue, pgtype.UUID{}, pgtype.UUID{}, nil)
		if cerr := tx.Commit(context.Background()); cerr != nil {
			t.Fatalf("commit: %v", cerr)
		}
		return err
	})
}

func TestAdmissionCoalescedSquadLeaderTxEnqueueIsRecordedWithProducerEvent(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	e.enqueue(issueID)
	issue := e.issueRow(issueID)
	wantCoalescedRecorded(t, e, issueID, func(ev string) error {
		tx, err := e.pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(context.Background())
		_, _, err = e.svc.EnqueueSquadLeaderRunInTx(WithEventID(context.Background(), ev), e.svc.Queries.WithTx(tx), issue, util.MustParseUUID(e.agent), pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{})
		if cerr := tx.Commit(context.Background()); cerr != nil {
			t.Fatalf("commit: %v", cerr)
		}
		return err
	})
}

func TestAdmissionCoalescedTriggerWithoutQueuedRunStoresNothing(t *testing.T) {
	e := newAdmEnv(t)
	issueID := e.issue("medium")
	issue := e.issueRow(issueID)
	if err := e.svc.RecordCoalescedTrigger(WithEventID(context.Background(), "evt-none"), issue, util.MustParseUUID(e.agent), pgtype.UUID{}, "status"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := e.eventIDs(issueID); len(got) != 0 {
		t.Fatalf("inputs = %v, want none: no queued run carries them", got)
	}
}
