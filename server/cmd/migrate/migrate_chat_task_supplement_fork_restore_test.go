package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHE-10692: the hand-run recovery script for the 99zzz00 fork conversion. The
// file path is the one the contract line in 99zzz00's up file names. Input: the
// attempt id A (32 hex) and the pre-deploy snapshot table chat_task_supplement_snapshot_<A>.

const (
	supplementRestoreScript  = "recovery/chat_task_supplement_fork_restore.sql"
	supplementRestoreAttempt = "0123456789abcdef0123456789abcdef"
)

// runSupplementRestore applies the script the way psql -v attempt_id=<A> does:
// it replaces the psql variable reference :'attempt_id' with a quoted literal.
func runSupplementRestore(ctx context.Context, pool *pgxpool.Pool, attempt string) error {
	raw, err := os.ReadFile(supplementRestoreScript)
	if err != nil {
		return err
	}
	sql := strings.ReplaceAll(string(raw), ":'attempt_id'", "'"+attempt+"'")
	_, err = pool.Exec(ctx, sql)
	return err
}

func mustExecSupplement(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func TestSupplementForkRestoreRecoversAfterPostDeployWrites(t *testing.T) {
	pool := supplementScratchDatabase(t)
	run := newSupplementRun(pool)
	ctx := t.Context()
	a := supplementRestoreAttempt
	m := func(n int) string { return supplementID("0000", n) }
	tk := func(n int) string { return supplementID("0001", n) }

	// Fork database: everything before S1 applied, 875/876 included.
	if err := run.up(t, supplementUpFiles(t, "", "99zzz")); err != nil {
		t.Fatalf("build fork baseline: %v", err)
	}
	seedForkSupplements(t, ctx, pool, "chat_task_supplement", supplementSeed())
	// The pre-deploy snapshot (N5a): the fork table before the migration.
	mustExecSupplement(t, ctx, pool, `CREATE TABLE chat_task_supplement_snapshot_`+a+` (LIKE chat_task_supplement INCLUDING DEFAULTS)`)
	mustExecSupplement(t, ctx, pool, `INSERT INTO chat_task_supplement_snapshot_`+a+` SELECT * FROM chat_task_supplement`)

	if err := run.up(t, supplementUpFiles(t, "", supplementLastUpstreamStep)); err != nil {
		t.Fatalf("deploy 99zzz00-99zzz04: %v", err)
	}

	// Post-deploy writes through the new image's upstream table.
	insertUpstream := func(message, task, status string) {
		mustExecSupplement(t, ctx, pool, `
			INSERT INTO chat_task_supplement (task_id, message_id, chat_session_id, workspace_id, author_id, status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())`,
			task, message, supplementID("5555", 90), wsA, supplementID("aaaa", 90), status)
	}
	insertUpstream(m(3), tk(4), "delivered")    // fork pending; the new image delivered it
	insertUpstream(m(4), tk(5), "withdrawn")    // fork delivering; withdrawn after deploy
	insertUpstream(m(20), tk(20), "pending")    // new, pending
	insertUpstream(m(21), tk(21), "requeued")   // new, requeued
	insertUpstream(m(22), tk(22), "delivered")  // new, delivered
	insertUpstream(m(23), tk(23), "delivering") // new, claimed but unacked
	insertUpstream(m(5), tk(80), "pending")     // fork has an uncertain row on another task: new pending row
	// Cross-task duplicate: fork (m2, t3) is delivered; the new image moves the message to task t50 as pending.
	// It must not come back as a second pending row.
	mustExecSupplement(t, ctx, pool,
		`UPDATE chat_task_supplement SET task_id = $1, status = 'pending' WHERE message_id = $2`, tk(50), m(2))
	// The contract ledger exists on the deployed path; the script removes only its 99zzz00 row.
	mustExecSupplement(t, ctx, pool, `CREATE TABLE schema_contracts (version TEXT PRIMARY KEY)`)
	mustExecSupplement(t, ctx, pool, `INSERT INTO schema_contracts (version) VALUES ('99zzz00_chat_task_supplement_fork_convert'), ('875_chat_task_supplement')`)

	var upstreamBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_task_supplement`).Scan(&upstreamBefore); err != nil {
		t.Fatal(err)
	}

	// Before: the recovery step is today's rollback path, which runs no data step. The
	// script is not on the tree yet, so this run is the red.
	if err := runSupplementRestore(ctx, pool, a); err != nil {
		t.Fatalf("restore script: %v", err)
	}

	// Fork shape is back: delivered_at exists and the fork's pkey and index names are restored.
	if !relationExists(t, ctx, pool, "chat_task_supplement_pkey") || !relationExists(t, ctx, pool, "chat_task_supplement_task_status_idx") {
		t.Error("fork index names not restored on chat_task_supplement")
	}
	var deliveredAtCols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'chat_task_supplement' AND column_name = 'delivered_at'`).Scan(&deliveredAtCols); err != nil || deliveredAtCols != 1 {
		t.Fatalf("chat_task_supplement is not the fork shape: delivered_at columns = %d, %v", deliveredAtCols, err)
	}

	// The previous image delivers only pending rows: exactly the two new pending writes.
	rows, err := pool.Query(ctx, `SELECT message_id::text, task_id::text FROM chat_task_supplement WHERE status = 'pending' ORDER BY message_id`)
	if err != nil {
		t.Fatal(err)
	}
	var pending []string
	for rows.Next() {
		var msg, task string
		if err := rows.Scan(&msg, &task); err != nil {
			t.Fatal(err)
		}
		pending = append(pending, msg+"/"+task)
	}
	rows.Close()
	wantPending := []string{m(5) + "/" + tk(80), m(20) + "/" + tk(20), m(21) + "/" + tk(21)}
	if !slices.Equal(pending, wantPending) {
		t.Fatalf("pending rows after restore = %v; want %v (a delivered row must not be pending again)", pending, wantPending)
	}

	var status, reason string
	if err := pool.QueryRow(ctx, `SELECT status, COALESCE(failure_reason, '') FROM chat_task_supplement WHERE message_id = $1 AND task_id = $2`, m(4), tk(5)).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason == "" {
		t.Fatalf("withdrawn-after-deploy row: status=%q reason=%q; want failed with a reason", status, reason)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM chat_task_supplement WHERE message_id = $1 AND task_id = $2`, m(23), tk(23)).Scan(&status); err != nil || status != "uncertain" {
		t.Fatalf("delivering-after-deploy row: status=%q, %v; want uncertain", status, err)
	}

	// The saved upstream table holds every row the new image wrote.
	saved := "chat_task_supplement_upstream_saved_" + a
	var savedCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{saved}.Sanitize()).Scan(&savedCount); err != nil || savedCount != upstreamBefore {
		t.Fatalf("saved upstream rows = %d, %v; want %d", savedCount, err, upstreamBefore)
	}

	// Each id has a report row with an outcome. The moved-task row is inserted, not pending, because the message was delivered on its old task.
	var outcome string
	if err := pool.QueryRow(ctx, `SELECT outcome FROM chat_task_supplement_restore_report WHERE attempt_id = $1 AND message_id = $2 AND task_id = $3`, a, m(2), tk(50)).Scan(&outcome); err != nil || outcome != "inserted_cross_task" {
		t.Fatalf("moved-task report outcome = %q, %v; want inserted_cross_task", outcome, err)
	}
	var movedStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM chat_task_supplement WHERE message_id = $1 AND task_id = $2`, m(2), tk(50)).Scan(&movedStatus); err != nil || movedStatus != "failed" {
		t.Fatalf("moved-task status = %q, %v; want failed (already delivered on another task)", movedStatus, err)
	}
	for _, c := range []struct {
		msg, task int
		outcome   string
	}{{3, 4, "fork_row_updated"}, {20, 20, "inserted"}, {22, 22, "inserted"}} {
		if err := pool.QueryRow(ctx, `SELECT outcome FROM chat_task_supplement_restore_report WHERE attempt_id = $1 AND message_id = $2 AND task_id = $3`, a, m(c.msg), tk(c.task)).Scan(&outcome); err != nil || outcome != c.outcome {
			t.Errorf("report outcome for message %d = %q, %v; want %s", c.msg, outcome, err, c.outcome)
		}
	}

	// The ledger no longer records 99zzz00-99zzz04, so a later up run applies them again.
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version IN ($1, $2, $3, $4, $5)`,
		supplementLastUpstreamStep, "99zzz00_chat_task_supplement_fork_convert", "99zzz01_chat_task_supplement",
		"99zzz02_chat_task_supplement_message", "99zzz03_chat_task_supplement_task").Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("ledger rows for 99zzz00-99zzz04 = %d, %v; want 0", ledger, err)
	}

	// A second run on the same attempt refuses: the saved tables already exist.
	if err := runSupplementRestore(ctx, pool, a); err == nil {
		t.Fatal("second restore on the same attempt succeeded; want refusal")
	}

	// The schema_contracts row of 99zzz00 is gone; the other row stays.
	var contracts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_contracts`).Scan(&contracts); err != nil || contracts != 1 {
		t.Fatalf("schema_contracts rows = %d, %v; want 1 (only 99zzz00 removed)", contracts, err)
	}

	// Green-only: after the restore the five migrations apply again.
	if err := run.up(t, supplementUpFiles(t, "", supplementLastUpstreamStep)); err != nil {
		t.Fatalf("re-up after restore: %v", err)
	}
}
