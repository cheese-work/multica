package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
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

// cappedRetryChild seeds a failed task that is a generic retry of a recovery
// task (cap rule 1) and returns its id. The accountable user is the fixture user.
func (o ownerFixture) cappedRetryChild(t *testing.T, over testutil.Cols) string {
	t.Helper()
	recovery := o.failedTask(t, nil)
	o.recordRecovery(t, o.failedTask(t, nil), db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, recovery)})
	cols := testutil.Cols{"retry_of_task_id": recovery, "accountable_user_id": o.UserID}
	for k, v := range over {
		cols[k] = v
	}
	id := o.failedTask(t, cols)
	o.Cleanup(t, "DELETE FROM inbox_item WHERE id=$1", id)
	return id
}

func (o ownerFixture) countNotices(t *testing.T, taskID string) int {
	t.Helper()
	var n int
	if err := o.Pool.QueryRow(context.Background(), `SELECT count(*) FROM inbox_item WHERE id=$1`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The legacy reservation reports a cap but writes and publishes nothing inside
// the caller's transaction; the caller sends the notice once the outcome is final.
func TestLegacyReservationCapNoticeWaitsForCallerOutcome(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	wakeupID := o.wakeup(t, "every", "continuous", nil)
	capped := o.cappedRetryChild(t, wakeupContext(t, wakeupID))
	published := 0
	o.svc.TaskSvc.Bus.Subscribe(protocol.EventInboxNew, func(events.Event) { published++ })
	attempt := &providerFailureRecoveryAttempt{
		FailedTaskID: parseTestUUID(t, capped), FailedAt: time.Now().UTC().Add(-2 * time.Hour),
		ProbeAt: time.Now().UTC(), ProbeStatus: http.StatusUnauthorized,
		Scope: providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: parseTestUUID(t, wakeupID)},
	}
	tx, err := o.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	reserved, err := o.svc.TaskSvc.reserveProviderFailureRecovery(ctx, db.New(tx), attempt, pgtype.UUID{}, dbid.NewV7())
	if err != nil || reserved {
		t.Fatalf("capped reservation reserved=%t err=%v", reserved, err)
	}
	var inTx int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM inbox_item WHERE id=$1`, capped).Scan(&inTx); err != nil || inTx != 0 || published != 0 {
		t.Fatalf("notice written in caller transaction=%d published=%d err=%v, want none", inTx, published, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	o.svc.TaskSvc.notifyProviderFailureAttemptCap(ctx, attempt)
	o.svc.TaskSvc.notifyProviderFailureAttemptCap(ctx, attempt)
	if n := o.countNotices(t, capped); n != 1 || published != 1 {
		t.Fatalf("after caller outcome notices=%d published=%d, want one each", n, published)
	}
}

// A recurring wakeup whose failed task is a retry of a recovery is capped: the
// dispatch skips it and the owner is told once, after the skip committed.
func TestRecurringWakeupCapTellsOwnerAfterSkipCommits(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	svc := &IssueWakeupService{Tasks: o.svc.TaskSvc}
	w := wakeCreate(t, o.principalFixture, svc, parseTestUUID(t, o.issueID), WakeupInput{
		AgentID: o.agentID, Kind: "every", Mode: "continuous", IntervalSeconds: 3600, Instruction: "cap",
	})
	capped := o.cappedRetryChild(t, wakeupContext(t, util.UUIDToString(w.ID)))
	o.Exec(t, "UPDATE agent_task_queue SET completed_at=now()-interval '90 minutes' WHERE id=$1", capped)
	o.svc.TaskSvc.ProviderFailureProbe = &providerFailureProbeStub{evidence: ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: http.StatusUnauthorized}}
	published := 0
	o.svc.TaskSvc.Bus.Subscribe(protocol.EventInboxNew, func(events.Event) { published++ })
	o.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", util.UUIDToString(w.ID))
	w, err := o.q.GetIssueWakeup(ctx, db.GetIssueWakeupParams{ID: w.ID, WorkspaceID: w.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.dispatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	var skipped int
	if err := o.Pool.QueryRow(ctx, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped'`, o.issueID).Scan(&skipped); err != nil || skipped != 1 {
		t.Fatalf("wakeup skips=%d err=%v, want 1", skipped, err)
	}
	if n := o.countNotices(t, capped); n != 1 || published != 1 {
		t.Fatalf("notices=%d published=%d, want one each", n, published)
	}
}

// A scheduled autopilot run that hits a cap rolls back its dispatch transaction;
// the owner notice must survive that rollback.
func TestScheduledAutopilotCapNoticeSurvivesDispatchRollback(t *testing.T) {
	for _, mode := range []string{"run_only", "create_issue"} {
		t.Run(mode, func(t *testing.T) {
			f, ownerID := newPrincipalFixture(t)
			o := ownerFixture{principalFixture: f}
			ctx := context.Background()
			agentID := f.privateAgentOwnedBy(t, ownerID, "cap-schedule")
			o.agentID = agentID
			autopilotID, triggerID := f.autopilotWithTrigger(t, agentID, ownerID, ownerID)
			f.Exec(t, "UPDATE autopilot SET execution_mode=$2 WHERE id=$1", autopilotID, mode)
			failedRun, err := f.q.CreateAutopilotRun(ctx, db.CreateAutopilotRunParams{
				ID: dbid.NewV7(), AutopilotID: parseTestUUID(t, autopilotID), TriggerID: parseTestUUID(t, triggerID),
				Source: "schedule", Status: "running", TriggerPayload: []byte(`{}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			f.Cleanup(t, "DELETE FROM autopilot_run WHERE id=$1", util.UUIDToString(failedRun.ID))
			noIssue := testutil.Cols{"issue_id": nil}
			recovery := o.failedTask(t, noIssue)
			o.recordRecovery(t, o.failedTask(t, noIssue), db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, recovery)})
			capped := o.failedTask(t, testutil.Cols{
				"retry_of_task_id": recovery, "accountable_user_id": ownerID, "issue_id": nil,
				"autopilot_run_id": util.UUIDToString(failedRun.ID), "completed_at": testutil.Raw("now()-interval '61 minutes'"),
			})
			f.Cleanup(t, "DELETE FROM inbox_item WHERE id=$1", capped)
			f.Exec(t, "UPDATE autopilot_run SET status='failed',completed_at=now() WHERE id=$1", util.UUIDToString(failedRun.ID))
			f.svc.TaskSvc.ProviderFailureProbe = &providerFailureProbeStub{evidence: ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: http.StatusUnauthorized}}
			published := 0
			f.svc.TaskSvc.Bus.Subscribe(protocol.EventInboxNew, func(events.Event) { published++ })

			run := f.dispatch(t, autopilotID, triggerID)
			if run.Status != "skipped" {
				t.Fatalf("run status = %s, want skipped by the cap", run.Status)
			}
			if n := o.countNotices(t, capped); n != 1 || published != 1 {
				t.Fatalf("notices=%d published=%d, want one each after rollback", n, published)
			}
		})
	}
}

func (o ownerFixture) taskRetryEffect(failedTaskID string) db.RecordProviderFailureRecoveryParams {
	return db.RecordProviderFailureRecoveryParams{
		FailedTaskID: util.MustParseUUID(failedTaskID), TriggerKind: "issue_wakeup", TriggerID: dbid.NewV7(),
		FailedAt:         pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		ChangedCondition: "recovery_reserved", RecoveryTaskID: dbid.NewV7(),
	}
}

// The seam hands a cap back to its caller instead of dropping it; the caller
// sends the one notice after the transaction commits.
func TestReserveSeamReturnsCapDecisionAndCallerNotifiesAfterCommit(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	capped := o.cappedRetryChild(t, nil)
	published := 0
	o.svc.TaskSvc.Bus.Subscribe(protocol.EventInboxNew, func(events.Event) { published++ })
	tx, err := o.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	applied := false
	reserved, decision, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.taskRetryEffect(capped), func(context.Context, pgx.Tx) error { applied = true; return nil })
	if err != nil || reserved || applied || !decision.Capped() || decision.Rule != 1 {
		t.Fatalf("reserved=%t applied=%t decision=%+v err=%v, want a rule 1 cap and no effect", reserved, applied, decision, err)
	}
	if published != 0 {
		t.Fatal("seam published a notice before the caller's outcome")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	o.svc.TaskSvc.notifyProviderFailureCap(ctx, o.q, parseTestUUID(t, capped), decision)
	o.svc.TaskSvc.notifyProviderFailureCap(ctx, o.q, parseTestUUID(t, capped), decision)
	if n := o.countNotices(t, capped); n != 1 || published != 1 {
		t.Fatalf("notices=%d published=%d, want one each", n, published)
	}
}

// Apply failure through the seam rolls back only the reservation and effect;
// the caller's own write still commits.
func TestReserveSeamApplyFailureKeepsOuterWrite(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	failed := o.failedTask(t, nil)
	tx, err := o.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE seam_outer_write (v text) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO seam_outer_write VALUES ('before')`); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("apply failed")
	reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.taskRetryEffect(failed), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO seam_outer_write VALUES ('inside-apply')`); err != nil {
			return err
		}
		return boom
	})
	if reserved || !errors.Is(err, boom) {
		t.Fatalf("reserved=%t err=%v, want the apply error", reserved, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO seam_outer_write VALUES ('after')`); err != nil {
		t.Fatalf("outer transaction unusable after apply failure: %v", err)
	}
	var rows []string
	r, err := tx.Query(ctx, `SELECT v FROM seam_outer_write ORDER BY v`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var v string
		if err := r.Scan(&v); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, v)
	}
	r.Close()
	if len(rows) != 2 || rows[0] != "after" || rows[1] != "before" {
		t.Fatalf("outer rows = %v, want [after before] (apply write rolled back)", rows)
	}
	var recovery int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id=$1`, failed).Scan(&recovery); err != nil || recovery != 0 {
		t.Fatalf("reservation rows=%d err=%v, want rolled back", recovery, err)
	}
}

// Two callers racing on one failed task through the seam: exactly one effect.
func TestReserveSeamConcurrentReservationYieldsOneEffect(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	failed := o.failedTask(t, nil)
	var wg sync.WaitGroup
	results := make([]bool, 8)
	errs := make([]error, len(results))
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := o.Pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			defer tx.Rollback(ctx)
			reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.taskRetryEffect(failed), func(context.Context, pgx.Tx) error { return nil })
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = tx.Commit(ctx)
			results[i] = reserved
		}()
	}
	wg.Wait()
	won := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if r {
			won++
		}
	}
	var rows int
	if err := o.Pool.QueryRow(ctx, `SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id=$1`, failed).Scan(&rows); err != nil || won != 1 || rows != 1 {
		t.Fatalf("winners=%d rows=%d err=%v, want one each", won, rows, err)
	}
}
