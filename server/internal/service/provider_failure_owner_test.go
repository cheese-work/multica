package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type ownerFixture struct {
	principalFixture
	issueID string
	agentID string
}

func newOwnerFixture(t *testing.T) ownerFixture {
	t.Helper()
	f, _, issue, agentID := wakeFixture(t)
	// The held provider retry (CHE-1736 M1.2) runs only on an issue still
	// assigned to the failed task's agent, so the fixture issue is.
	if _, err := f.Pool.Exec(context.Background(), `UPDATE issue SET assignee_type='agent', assignee_id=$2 WHERE id=$1`, issue, agentID); err != nil {
		t.Fatal(err)
	}
	return ownerFixture{principalFixture: f, issueID: util.UUIDToString(issue), agentID: agentID}
}

func (o ownerFixture) wakeup(t *testing.T, kind, mode string, systemRule any) string {
	t.Helper()
	cols := testutil.Cols{
		"id": util.UUIDToString(dbid.NewV7()), "workspace_id": o.WorkspaceID, "issue_id": o.issueID,
		"instruction": "owner test", "kind": kind, "mode": mode,
	}
	if systemRule != nil {
		cols["system_rule"] = systemRule
	}
	id := o.Insert(t, "issue_wakeup", cols)
	o.Cleanup(t, "DELETE FROM provider_failure_recovery WHERE recovery_wakeup_id=$1", id)
	return id
}

// failedTask queues a provider-failed task on the fixture issue.
func (o ownerFixture) failedTask(t *testing.T, over testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"runtime_id":     testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + o.agentID + "')"),
		"issue_id":       o.issueID,
		"status":         "failed",
		"failure_reason": "agent_error.provider_server_error",
		"completed_at":   testutil.Raw("now()"),
	}
	for k, v := range over {
		cols[k] = v
	}
	id := o.Task(t, o.agentID, cols)
	o.Cleanup(t, "DELETE FROM provider_failure_recovery WHERE failed_task_id=$1 OR recovery_task_id=$1", id)
	return id
}

func wakeupContext(t *testing.T, wakeupID string) testutil.Cols {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"wakeup_id": wakeupID})
	if err != nil {
		t.Fatal(err)
	}
	return testutil.Cols{"context": testutil.Raw(fmt.Sprintf("'%s'::jsonb", payload))}
}

func (o ownerFixture) recordRecovery(t *testing.T, failedTaskID string, over db.RecordProviderFailureRecoveryParams) {
	t.Helper()
	over.FailedTaskID = parseTestUUID(t, failedTaskID)
	over.TriggerKind = "issue_wakeup"
	over.TriggerID = dbid.NewV7()
	over.FailedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	over.ChangedCondition = "recovery_reserved"
	if rows, err := o.q.RecordProviderFailureRecovery(context.Background(), over); err != nil || rows != 1 {
		t.Fatalf("record recovery rows=%d err=%v", rows, err)
	}
}

func TestResolveProviderFailureOwner(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()

	// Guard-owned fixtures.
	scheduleAgent := o.agentID
	autopilotID, triggerID := o.autopilotWithTrigger(t, scheduleAgent, o.UserID, o.UserID)
	scheduleRun := func(source string) string {
		run, err := o.q.CreateAutopilotRun(ctx, db.CreateAutopilotRunParams{
			ID: dbid.NewV7(), AutopilotID: parseTestUUID(t, autopilotID), TriggerID: parseTestUUID(t, triggerID),
			Source: source, Status: "running", TriggerPayload: []byte(`{}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		o.Cleanup(t, "DELETE FROM autopilot_run WHERE id=$1", util.UUIDToString(run.ID))
		return util.UUIDToString(run.ID)
	}
	fallbackIssue := o.Issue(t, "autopilot fallback")
	o.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", fallbackIssue)
	o.Exec(t, "UPDATE autopilot_run SET issue_id=$2 WHERE id=$1", scheduleRun("schedule"), fallbackIssue)

	// A failed recovery task, a generic-retry child of it, and a parent-linked child.
	recoverySource := o.failedTask(t, nil)
	recoveryTask := o.failedTask(t, nil)
	o.recordRecovery(t, recoverySource, db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, recoveryTask)})
	retryChild := o.failedTask(t, testutil.Cols{"retry_of_task_id": recoveryTask})
	parentChild := o.failedTask(t, testutil.Cols{"parent_task_id": retryChild})

	cappedWakeup := o.wakeup(t, "at", "once", nil)
	o.recordRecovery(t, o.failedTask(t, nil), db.RecordProviderFailureRecoveryParams{
		RecoveryWakeupID: parseTestUUID(t, cappedWakeup), RecoveryWakeupRevision: pgtype.Int8{Int64: 1, Valid: true},
	})

	triageIssue := o.Issue(t, "triage", testutil.Cols{"triage_state": "pending"})
	o.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", triageIssue)

	for _, tc := range []struct {
		name string
		task string
		want providerFailureOwner
		rule int
	}{
		{"1 failed recovery task", recoveryTask, providerOwnerNone, 1},
		{"1 generic retry of recovery task", retryChild, providerOwnerNone, 1},
		{"1 parent-linked descendant", parentChild, providerOwnerNone, 1},
		{"2 wakeup already re-armed", o.failedTask(t, wakeupContext(t, cappedWakeup)), providerOwnerNone, 2},
		{"3 continuous every", o.failedTask(t, wakeupContext(t, o.wakeup(t, "every", "continuous", nil))), providerOwnerNone, 3},
		{"3 continuous cron", o.failedTask(t, wakeupContext(t, o.wakeup(t, "cron", "continuous", nil))), providerOwnerNone, 3},
		{"3 schedule autopilot by run id", o.failedTask(t, testutil.Cols{"autopilot_run_id": scheduleRun("schedule")}), providerOwnerNone, 3},
		{"3 schedule autopilot by issue fallback", o.failedTask(t, testutil.Cols{"issue_id": fallbackIssue}), providerOwnerNone, 3},
		{"4 once at", o.failedTask(t, wakeupContext(t, o.wakeup(t, "at", "once", nil))), providerOwnerWakeupRearm, 4},
		{"4 once event", o.failedTask(t, wakeupContext(t, o.wakeup(t, "event", "once", nil))), providerOwnerWakeupRearm, 4},
		{"5 continuous event", o.failedTask(t, wakeupContext(t, o.wakeup(t, "event", "continuous", nil))), providerOwnerTaskRetry, 5},
		{"5 system-rule once", o.failedTask(t, wakeupContext(t, o.wakeup(t, "event", "once", "child_done"))), providerOwnerTaskRetry, 5},
		{"6 no issue", o.failedTask(t, testutil.Cols{"issue_id": nil}), providerOwnerNone, 6},
		{"6 non-schedule autopilot run", o.failedTask(t, testutil.Cols{"autopilot_run_id": scheduleRun("manual")}), providerOwnerNone, 6},
		{"6 triage issue", o.failedTask(t, testutil.Cols{"issue_id": triageIssue}), providerOwnerNone, 6},
		{"7 plain issue task", o.failedTask(t, nil), providerOwnerTaskRetry, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProviderFailureOwner(ctx, o.q, parseTestUUID(t, tc.task))
			if err != nil {
				t.Fatal(err)
			}
			if got.Owner != tc.want || got.Rule != tc.rule {
				t.Fatalf("owner = %+v, want %s by rule %d", got, tc.want, tc.rule)
			}
		})
	}
}

func TestProviderFailureEffectKindFollowsColumnShape(t *testing.T) {
	probeAt, probeStatus := recoveryProbeEvidence()
	for name, tc := range map[string]struct {
		params db.RecordProviderFailureRecoveryParams
		want   providerFailureOwner
	}{
		"task effect":   {db.RecordProviderFailureRecoveryParams{ChangedCondition: "recovery_reserved", RecoveryTaskID: dbid.NewV7()}, providerOwnerTaskRetry},
		"wakeup effect": {db.RecordProviderFailureRecoveryParams{ChangedCondition: "recovery_reserved", RecoveryWakeupID: dbid.NewV7()}, providerOwnerWakeupRearm},
		"guard task":    {db.RecordProviderFailureRecoveryParams{ChangedCondition: providerFailureChangedState, ProbeAt: probeAt, ProbeStatus: probeStatus, RecoveryTaskID: dbid.NewV7()}, providerOwnerNone},
		"guard run":     {db.RecordProviderFailureRecoveryParams{ChangedCondition: providerFailureChangedState, ProbeAt: probeAt, ProbeStatus: probeStatus, RecoveryRunID: dbid.NewV7()}, providerOwnerNone},
		"empty":         {db.RecordProviderFailureRecoveryParams{ChangedCondition: "recovery_reserved"}, providerOwnerNone},
	} {
		t.Run(name, func(t *testing.T) {
			if got := providerFailureEffectKind(tc.params); got != tc.want {
				t.Fatalf("effect kind = %s, want %s", got, tc.want)
			}
		})
	}
}

// The seam reserves only the effect the canonical owner names; a capped,
// guard-owned or other-kind call inserts nothing and never applies.
func TestReserveProviderFailureRecoveryAndApplyRequiresOwner(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	pool := o.Pool
	cappedSource := o.failedTask(t, nil)
	cappedRecovery := o.failedTask(t, nil)
	o.recordRecovery(t, cappedSource, db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, cappedRecovery)})
	retryOfRecovery := o.failedTask(t, testutil.Cols{"retry_of_task_id": cappedRecovery})
	rearmWakeup := o.wakeup(t, "at", "once", nil)

	for _, tc := range []struct {
		name   string
		failed string
		params db.RecordProviderFailureRecoveryParams
		want   bool
	}{
		{"task_retry owner, task effect", o.failedTask(t, nil), db.RecordProviderFailureRecoveryParams{RecoveryTaskID: dbid.NewV7()}, true},
		{"wakeup_rearm owner, wakeup effect", o.failedTask(t, wakeupContext(t, rearmWakeup)),
			db.RecordProviderFailureRecoveryParams{RecoveryWakeupID: parseTestUUID(t, rearmWakeup), RecoveryWakeupRevision: pgtype.Int8{Int64: 1, Valid: true}}, true},
		{"task_retry owner, wakeup effect", o.failedTask(t, nil),
			db.RecordProviderFailureRecoveryParams{RecoveryWakeupID: dbid.NewV7(), RecoveryWakeupRevision: pgtype.Int8{Int64: 1, Valid: true}}, false},
		{"wakeup_rearm owner, task effect", o.failedTask(t, wakeupContext(t, o.wakeup(t, "event", "once", nil))), db.RecordProviderFailureRecoveryParams{RecoveryTaskID: dbid.NewV7()}, false},
		{"capped owner none", retryOfRecovery, db.RecordProviderFailureRecoveryParams{RecoveryTaskID: dbid.NewV7()}, false},
		{"unknown failed task", util.UUIDToString(dbid.NewV7()), db.RecordProviderFailureRecoveryParams{RecoveryTaskID: dbid.NewV7()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.params.FailedTaskID = parseTestUUID(t, tc.failed)
			tc.params.TriggerKind = "issue_wakeup"
			tc.params.TriggerID = dbid.NewV7()
			tc.params.FailedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
			tc.params.ChangedCondition = "recovery_reserved"
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			applied := false
			got, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, tc.params, func(context.Context, pgx.Tx) error { applied = true; return nil })
			if err != nil || got != tc.want || applied != tc.want {
				t.Fatalf("reserved=%t applied=%t err=%v, want %t", got, applied, err, tc.want)
			}
			var rows int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id=$1`, tc.params.FailedTaskID).Scan(&rows); err != nil {
				t.Fatalf("caller transaction unusable after refusal: %v", err)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.want]; rows != want && tc.name != "capped owner none" {
				t.Fatalf("reservation rows = %d, want %d", rows, want)
			}
		})
	}
}

// A capped failed task is never reserved by the legacy guard path either.
func TestLegacyProviderFailureReservationHonorsCaps(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	wakeupID := o.wakeup(t, "every", "continuous", nil)
	source := o.failedTask(t, wakeupContext(t, wakeupID))
	recovery := o.failedTask(t, wakeupContext(t, wakeupID))
	o.recordRecovery(t, source, db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, recovery)})
	retryChild := o.failedTask(t, testutil.Cols{
		"retry_of_task_id": recovery, "context": wakeupContext(t, wakeupID)["context"],
		"completed_at": testutil.Raw("now()+interval '1 minute'"),
	})
	attempt := &providerFailureRecoveryAttempt{
		FailedTaskID: parseTestUUID(t, retryChild), FailedAt: time.Now().UTC().Add(-2 * time.Hour),
		ProbeAt: time.Now().UTC(), ProbeStatus: 401,
		Scope: providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: parseTestUUID(t, wakeupID)},
	}
	svc := &TaskService{Queries: o.q}
	reserved, err := svc.reserveProviderFailureRecovery(ctx, o.q, attempt, pgtype.UUID{}, dbid.NewV7())
	if err != nil || reserved {
		t.Fatalf("capped legacy reservation reserved=%t err=%v", reserved, err)
	}
}

// A duplicate effect is refused at the database boundary without aborting the
// caller's transaction.
func TestProviderFailureReservationRefusesDuplicateEffectAtDatabase(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	source := o.failedTask(t, nil)
	recovery := o.failedTask(t, nil)
	o.recordRecovery(t, source, db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, recovery)})
	dupSource := o.failedTask(t, nil)
	tx, err := o.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, err := db.New(tx).RecordProviderFailureRecovery(ctx, db.RecordProviderFailureRecoveryParams{
		FailedTaskID: parseTestUUID(t, dupSource), TriggerKind: "issue_wakeup", TriggerID: dbid.NewV7(),
		FailedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}, ChangedCondition: "recovery_reserved",
		RecoveryTaskID: parseTestUUID(t, recovery),
	})
	if err != nil || rows != 0 {
		t.Fatalf("duplicate recovery task rows=%d err=%v, want refusal", rows, err)
	}
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("caller transaction aborted: %v", err)
	}
}

// A capped failed task tells its accountable user once, in issue-run wording,
// on the existing unconstrained inbox type. Other owner decisions send nothing.
func TestProviderFailureCapNoticeIsOnePerFailedTask(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	recovery := o.failedTask(t, nil)
	o.recordRecovery(t, o.failedTask(t, nil), db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, recovery)})
	capped := o.failedTask(t, testutil.Cols{"retry_of_task_id": recovery, "accountable_user_id": o.UserID})
	uncapped := o.failedTask(t, testutil.Cols{"accountable_user_id": o.UserID})
	o.Cleanup(t, "DELETE FROM inbox_item WHERE id=ANY($1::uuid[])", []string{capped, uncapped})

	published := 0
	o.svc.TaskSvc.Bus.Subscribe(protocol.EventInboxNew, func(events.Event) { published++ })
	for _, task := range []string{capped, capped, uncapped} {
		decision, err := resolveProviderFailureOwner(ctx, o.q, parseTestUUID(t, task))
		if err != nil {
			t.Fatal(err)
		}
		o.svc.TaskSvc.notifyProviderFailureCap(ctx, o.q, parseTestUUID(t, task), decision)
	}
	var count int
	if err := o.Pool.QueryRow(ctx, `SELECT count(*) FROM inbox_item WHERE id=ANY($1::uuid[])`, []string{capped, uncapped}).Scan(&count); err != nil || count != 1 {
		t.Fatalf("notices = %d err=%v, want exactly one for the capped task", count, err)
	}
	var recipient, noticeType, severity, body, reason string
	var visible bool
	if err := o.Pool.QueryRow(ctx, `SELECT recipient_id::text, type, severity, body, details->>'reason_code',
			inbox_item_visible(type, chat_session_id, workspace_id, recipient_id)
		FROM inbox_item WHERE id=$1`, capped).Scan(&recipient, &noticeType, &severity, &body, &reason, &visible); err != nil {
		t.Fatal(err)
	}
	if recipient != o.UserID || noticeType != "provider_failure_recovery_blocked" || severity != "action_required" || !visible {
		t.Fatalf("notice recipient=%s type=%s severity=%s visible=%t", recipient, noticeType, severity, visible)
	}
	if !strings.Contains(body, "run") || strings.Contains(body, "recurring trigger") || reason != "provider_failure_recovery_capped" {
		t.Fatalf("notice body=%q reason=%q, want issue-run wording", body, reason)
	}
	if published != 1 {
		t.Fatalf("inbox events = %d, want one", published)
	}
}

// task_retry rows reuse the legacy trigger columns as provenance only: a task
// that came from a wakeup names that wakeup, an ordinary issue task names itself.
// Neither invents a trigger, and the recurring-trigger guard never reads the row
// by trigger_id.
func TestProviderFailureTriggerMapping(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	wakeupID := o.wakeup(t, "every", "continuous", nil)
	fromWakeup := o.failedTask(t, wakeupContext(t, wakeupID))
	ordinary := o.failedTask(t, nil)
	for name, tc := range map[string]struct{ task, wantID string }{
		"wakeup task names its wakeup":     {fromWakeup, wakeupID},
		"ordinary issue task names itself": {ordinary, ordinary},
	} {
		t.Run(name, func(t *testing.T) {
			kind, id, err := providerFailureTrigger(ctx, o.q, parseTestUUID(t, tc.task))
			if err != nil || kind != "issue_wakeup" || util.UUIDToString(id) != tc.wantID {
				t.Fatalf("trigger = %s %s err=%v, want issue_wakeup %s", kind, util.UUIDToString(id), err, tc.wantID)
			}
			o.recordRecovery(t, tc.task, db.RecordProviderFailureRecoveryParams{RecoveryTaskID: dbid.NewV7()})
		})
	}
	if _, _, err := providerFailureTrigger(ctx, o.q, dbid.NewV7()); err == nil {
		t.Fatal("unknown failed task must not get a trigger")
	}
}
