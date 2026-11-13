package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-1732 M0c: one timeline receipt per recovery effect, written in the same
// savepoint as the reservation and the effect.

type receiptRow struct {
	Action  string
	Details map[string]any
}

func (o ownerFixture) receipts(t *testing.T, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) []receiptRow {
	t.Helper()
	rows, err := q.Query(context.Background(),
		`SELECT action, details FROM activity_log
		 WHERE issue_id=$1 AND action IN ('provider_recovery_reserved','wakeup_rearmed')
		 ORDER BY created_at, id`, o.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []receiptRow
	for rows.Next() {
		var action string
		var raw []byte
		if err := rows.Scan(&action, &raw); err != nil {
			t.Fatal(err)
		}
		d := map[string]any{}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, receiptRow{action, d})
	}
	return out
}

func (o ownerFixture) wakeupRearmEffect(failedTaskID, wakeupID string, revision int64) db.RecordProviderFailureRecoveryParams {
	return db.RecordProviderFailureRecoveryParams{
		FailedTaskID: util.MustParseUUID(failedTaskID), TriggerKind: "issue_wakeup", TriggerID: dbid.NewV7(),
		FailedAt:         pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		ChangedCondition: "recovery_reserved",
		RecoveryWakeupID: util.MustParseUUID(wakeupID), RecoveryWakeupRevision: pgtype.Int8{Int64: revision, Valid: true},
	}
}

func (o ownerFixture) begin(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := o.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func (o ownerFixture) cleanupReceipts(t *testing.T) {
	t.Helper()
	o.Cleanup(t, "DELETE FROM activity_log WHERE issue_id=$1 AND action IN ('provider_recovery_reserved','wakeup_rearmed')", o.issueID)
}

func TestRecoveryReceiptTaskRetryCommitsWithEffect(t *testing.T) {
	o := newOwnerFixture(t)
	o.cleanupReceipts(t)
	ctx := context.Background()
	failed := o.failedTask(t, nil)
	effect := o.taskRetryEffect(failed)
	tx := o.begin(t)
	reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, effect, func(context.Context, pgx.Tx) error { return nil })
	if err != nil || !reserved {
		t.Fatalf("reserved=%t err=%v, want reserved", reserved, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := o.receipts(t, o.Pool)
	if len(got) != 1 || got[0].Action != "provider_recovery_reserved" {
		t.Fatalf("receipts = %+v, want exactly one provider_recovery_reserved (a task effect writes no wakeup_rearmed)", got)
	}
	d := got[0].Details
	if d["failed_task_id"] != failed || d["effect_kind"] != "task_retry" ||
		d["effect_ref"] != util.UUIDToString(effect.RecoveryTaskID) || d["reservation"] != failed {
		t.Fatalf("details = %v, want failed %s, task_retry, ref %s", d, failed, util.UUIDToString(effect.RecoveryTaskID))
	}
	if _, has := d["wakeup_revision"]; has {
		t.Fatalf("task effect carries wakeup_revision: %v", d)
	}
}

func TestRecoveryReceiptWakeupRearmWritesBothRows(t *testing.T) {
	o := newOwnerFixture(t)
	o.cleanupReceipts(t)
	ctx := context.Background()
	wakeup := o.wakeup(t, "at", "once", nil)
	failed := o.failedTask(t, wakeupContext(t, wakeup))
	tx := o.begin(t)
	reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.wakeupRearmEffect(failed, wakeup, 3), func(context.Context, pgx.Tx) error { return nil })
	if err != nil || !reserved {
		t.Fatalf("reserved=%t err=%v, want reserved", reserved, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := o.receipts(t, o.Pool)
	if len(got) != 2 || got[0].Action != "provider_recovery_reserved" || got[1].Action != "wakeup_rearmed" {
		t.Fatalf("receipts = %+v, want provider_recovery_reserved then wakeup_rearmed", got)
	}
	r := got[0].Details
	if r["failed_task_id"] != failed || r["effect_kind"] != "wakeup_rearm" || r["effect_ref"] != wakeup || r["wakeup_revision"] != float64(3) {
		t.Fatalf("receipt details = %v", r)
	}
	g := got[1].Details
	if g["failed_task_id"] != failed || g["wakeup_revision"] != float64(3) {
		t.Fatalf("generation event details = %v, want failed_task_id and revision 3", g)
	}
	if w, _ := g["wakeup"].(map[string]any); w["id"] != wakeup {
		t.Fatalf("generation event preview = %v, want wakeup %s", g["wakeup"], wakeup)
	}
}

func TestRecoveryReceiptRollsBackWithOuterTransaction(t *testing.T) {
	o := newOwnerFixture(t)
	o.cleanupReceipts(t)
	ctx := context.Background()
	wakeup := o.wakeup(t, "at", "once", nil)
	failedWake := o.failedTask(t, wakeupContext(t, wakeup))
	failedTask := o.failedTask(t, nil)
	tx := o.begin(t)
	for _, effect := range []db.RecordProviderFailureRecoveryParams{o.wakeupRearmEffect(failedWake, wakeup, 1), o.taskRetryEffect(failedTask)} {
		if reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, effect, func(context.Context, pgx.Tx) error { return nil }); err != nil || !reserved {
			t.Fatalf("reserved=%t err=%v", reserved, err)
		}
	}
	if n := len(o.receipts(t, tx)); n != 3 {
		t.Fatalf("receipts inside the transaction = %d, want 3", n)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(o.receipts(t, o.Pool)); n != 0 {
		t.Fatalf("receipts after rollback = %d, want 0", n)
	}
}

func TestRecoveryReceiptAbsentWhenApplyFails(t *testing.T) {
	o := newOwnerFixture(t)
	o.cleanupReceipts(t)
	ctx := context.Background()
	failed := o.failedTask(t, nil)
	tx := o.begin(t)
	boom := errors.New("apply failed")
	reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.taskRetryEffect(failed), func(context.Context, pgx.Tx) error { return boom })
	if reserved || !errors.Is(err, boom) {
		t.Fatalf("reserved=%t err=%v, want the apply error", reserved, err)
	}
	if n := len(o.receipts(t, tx)); n != 0 {
		t.Fatalf("receipts = %d after a failed apply, want 0 (outer transaction must stay usable)", n)
	}
}

func TestRecoveryReceiptAbsentWhenNothingReserved(t *testing.T) {
	o := newOwnerFixture(t)
	o.cleanupReceipts(t)
	ctx := context.Background()
	ok := func(context.Context, pgx.Tx) error { return nil }

	// Owner none: a capped retry child.
	capped := o.cappedRetryChild(t, nil)
	tx := o.begin(t)
	if reserved, d, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.taskRetryEffect(capped), ok); err != nil || reserved || d.Owner != providerOwnerNone {
		t.Fatalf("capped: reserved=%t decision=%+v err=%v", reserved, d, err)
	}
	// Owner mismatch: a task-retry owner asked for a wakeup re-arm.
	wakeup := o.wakeup(t, "event", "continuous", nil)
	mismatch := o.failedTask(t, wakeupContext(t, wakeup))
	if reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.wakeupRearmEffect(mismatch, wakeup, 1), ok); err != nil || reserved {
		t.Fatalf("mismatch: reserved=%t err=%v", reserved, err)
	}
	// Reservation insert returns zero rows (duplicate failed task).
	dup := o.failedTask(t, nil)
	if reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.taskRetryEffect(dup), ok); err != nil || !reserved {
		t.Fatalf("first dup: reserved=%t err=%v", reserved, err)
	}
	before := len(o.receipts(t, tx))
	if reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, o.taskRetryEffect(dup), ok); err != nil || reserved {
		t.Fatalf("second dup: reserved=%t err=%v", reserved, err)
	}
	if after := len(o.receipts(t, tx)); after != before || before != 1 {
		t.Fatalf("receipts before=%d after=%d, want one from the winning reservation only", before, after)
	}
}

// A real failure, reservation, effect and receipt share one transaction.
// Commit keeps all four; rollback leaves none, including the failure itself.
func TestRecoveryReceiptSharesTransactionWithFailureReservationAndRealEffect(t *testing.T) {
	for _, kind := range []providerFailureOwner{providerOwnerTaskRetry, providerOwnerWakeupRearm} {
		for _, commit := range []bool{true, false} {
			name := string(kind) + map[bool]string{true: "/commit", false: "/rollback"}[commit]
			t.Run(name, func(t *testing.T) {
				o := newOwnerFixture(t)
				o.cleanupReceipts(t)
				ctx := context.Background()
				var wakeup string
				var failed string
				var effect db.RecordProviderFailureRecoveryParams
				if kind == providerOwnerWakeupRearm {
					wakeup = o.wakeup(t, "at", "once", nil)
					failed = o.failedTask(t, wakeupContext(t, wakeup))
					effect = o.wakeupRearmEffect(failed, wakeup, 2)
				} else {
					failed = o.failedTask(t, nil)
					effect = o.taskRetryEffect(failed)
				}
				// The task is still running; the transaction below is what fails it.
				if _, err := o.Pool.Exec(ctx, `UPDATE agent_task_queue SET status='running', started_at=now(), completed_at=NULL, failure_reason=NULL WHERE id=$1`, failed); err != nil {
					t.Fatal(err)
				}
				effectTask := util.UUIDToString(effect.RecoveryTaskID)
				if kind == providerOwnerTaskRetry {
					o.Cleanup(t, "DELETE FROM agent_task_queue WHERE id=$1", effectTask)
				}

				tx := o.begin(t)
				if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET status='failed', failure_reason='agent_error.provider_server_error', completed_at=now() WHERE id=$1`, failed); err != nil {
					t.Fatal(err)
				}
				reserved, _, err := reserveProviderFailureRecoveryAndApply(ctx, tx, effect, func(ctx context.Context, sp pgx.Tx) error {
					if kind == providerOwnerWakeupRearm {
						_, err := sp.Exec(ctx, `UPDATE issue_wakeup SET next_fire_at=now()+interval '1 minute', revision=2 WHERE id=$1`, wakeup)
						return err
					}
					_, err := db.New(sp).CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: effect.FailedTaskID, NewTaskID: effect.RecoveryTaskID})
					return err
				})
				if err != nil || !reserved {
					t.Fatalf("reserved=%t err=%v, want reserved", reserved, err)
				}
				end := tx.Rollback
				if commit {
					end = tx.Commit
				}
				if err := end(ctx); err != nil {
					t.Fatal(err)
				}
				var status string
				var reservations, retryChildren, rearmed int
				scan := func(sql string, dst any, args ...any) {
					t.Helper()
					if err := o.Pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
						t.Fatal(err)
					}
				}
				scan(`SELECT status FROM agent_task_queue WHERE id=$1`, &status, failed)
				scan(`SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id=$1`, &reservations, failed)
				if kind == providerOwnerTaskRetry {
					scan(`SELECT count(*) FROM agent_task_queue WHERE id=$1`, &retryChildren, effectTask)
				}
				if wakeup != "" {
					scan(`SELECT count(*) FROM issue_wakeup WHERE id=$1 AND revision=2 AND next_fire_at IS NOT NULL`, &rearmed, wakeup)
				}
				wantRows := map[providerFailureOwner]int{providerOwnerTaskRetry: 1, providerOwnerWakeupRearm: 2}[kind]
				got := o.receipts(t, o.Pool)
				if commit {
					wantEffect := map[providerFailureOwner]int{providerOwnerTaskRetry: 1, providerOwnerWakeupRearm: 0}[kind]
					wantRearmed := map[providerFailureOwner]int{providerOwnerTaskRetry: 0, providerOwnerWakeupRearm: 1}[kind]
					if status != "failed" || reservations != 1 || retryChildren != wantEffect || rearmed != wantRearmed || len(got) != wantRows {
						t.Fatalf("after commit: status=%s reservations=%d retryChildren=%d rearmed=%d receipts=%+v, want failed, 1, %d, %d, %d receipts",
							status, reservations, retryChildren, rearmed, got, wantEffect, wantRearmed, wantRows)
					}
				} else if status != "running" || reservations != 0 || retryChildren != 0 || rearmed != 0 || len(got) != 0 {
					t.Fatalf("after rollback: status=%s reservations=%d retryChildren=%d rearmed=%d receipts=%+v, want running and nothing else",
						status, reservations, retryChildren, rearmed, got)
				}
			})
		}
	}
}
