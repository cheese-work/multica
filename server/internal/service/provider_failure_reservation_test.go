package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type providerFailureReservation struct {
	failedTaskID pgtype.UUID
	triggerID    pgtype.UUID
	probeAt      pgtype.Timestamptz
	probeStatus  pgtype.Int4
	changed      string
	runID        pgtype.UUID
	taskID       pgtype.UUID
	wakeupID     pgtype.UUID
	wakeupRev    pgtype.Int8
}

type providerFailureExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func insertProviderFailureReservation(ctx context.Context, exec providerFailureExecutor, reservation providerFailureReservation, ignoreDuplicate bool) (int64, error) {
	query := `
INSERT INTO provider_failure_recovery (
    failed_task_id, trigger_kind, trigger_id, condition_key, failed_at,
    probe_at, probe_status, changed_condition, recovery_run_id,
    recovery_task_id, recovery_wakeup_id, recovery_wakeup_revision
)
VALUES ($1, 'issue_wakeup', $2, '', now(), $3, $4, $5, $6, $7, $8, $9)`
	if ignoreDuplicate {
		query += ` ON CONFLICT (failed_task_id) DO NOTHING`
	}
	tag, err := exec.Exec(ctx, query,
		reservation.failedTaskID,
		reservation.triggerID,
		reservation.probeAt,
		reservation.probeStatus,
		reservation.changed,
		reservation.runID,
		reservation.taskID,
		reservation.wakeupID,
		reservation.wakeupRev,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func newProviderFailureReservation(changed string) providerFailureReservation {
	return providerFailureReservation{
		failedTaskID: dbid.NewV7(),
		triggerID:    dbid.NewV7(),
		changed:      changed,
	}
}

func providerFailureReservationParams(reservation providerFailureReservation) db.RecordProviderFailureRecoveryParams {
	return db.RecordProviderFailureRecoveryParams{
		FailedTaskID:           reservation.failedTaskID,
		TriggerKind:            "issue_wakeup",
		TriggerID:              reservation.triggerID,
		ConditionKey:           "",
		FailedAt:               pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		ProbeAt:                reservation.probeAt,
		ProbeStatus:            reservation.probeStatus,
		ChangedCondition:       reservation.changed,
		RecoveryRunID:          reservation.runID,
		RecoveryTaskID:         reservation.taskID,
		RecoveryWakeupID:       reservation.wakeupID,
		RecoveryWakeupRevision: reservation.wakeupRev,
	}
}

func recoveryProbeEvidence() (pgtype.Timestamptz, pgtype.Int4) {
	return pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}, pgtype.Int4{Int32: 401, Valid: true}
}

func TestProviderFailureRecoveryEffectShapesPreserveLegacyRows(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	probeAt, probeStatus := recoveryProbeEvidence()
	legacyRun := newProviderFailureReservation(providerFailureChangedState)
	legacyRun.runID = dbid.NewV7()
	legacyRun.probeAt, legacyRun.probeStatus = probeAt, probeStatus
	legacyTask := newProviderFailureReservation(providerFailureChangedState)
	legacyTask.taskID = dbid.NewV7()
	legacyTask.probeAt, legacyTask.probeStatus = probeAt, probeStatus
	taskRetry := newProviderFailureReservation("recovery_reserved")
	taskRetry.taskID = dbid.NewV7()
	wakeupRearm := newProviderFailureReservation("recovery_reserved")
	wakeupRearm.wakeupID = dbid.NewV7()
	wakeupRearm.wakeupRev = pgtype.Int8{Int64: 2, Valid: true}

	for _, reservation := range []providerFailureReservation{legacyRun, legacyTask, taskRetry, wakeupRearm} {
		reservation := reservation
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM provider_failure_recovery WHERE failed_task_id=$1`, reservation.failedTaskID)
		})
		if _, err := insertProviderFailureReservation(ctx, pool, reservation, false); err != nil {
			t.Fatalf("valid reservation shape rejected: changed=%q task=%t run=%t wakeup=%t: %v",
				reservation.changed, reservation.taskID.Valid, reservation.runID.Valid, reservation.wakeupID.Valid, err)
		}
	}

	invalid := []struct {
		name        string
		reservation providerFailureReservation
	}{
		{name: "no effect", reservation: newProviderFailureReservation("recovery_reserved")},
		{name: "two legacy effects", reservation: func() providerFailureReservation {
			reservation := newProviderFailureReservation(providerFailureChangedState)
			reservation.runID, reservation.taskID = dbid.NewV7(), dbid.NewV7()
			reservation.probeAt, reservation.probeStatus = probeAt, probeStatus
			return reservation
		}()},
		{name: "wakeup rearm without revision", reservation: func() providerFailureReservation {
			reservation := newProviderFailureReservation("recovery_reserved")
			reservation.wakeupID = dbid.NewV7()
			return reservation
		}()},
		{name: "task retry and wakeup rearm", reservation: func() providerFailureReservation {
			reservation := newProviderFailureReservation("recovery_reserved")
			reservation.taskID = dbid.NewV7()
			reservation.wakeupID = dbid.NewV7()
			reservation.wakeupRev = pgtype.Int8{Int64: 2, Valid: true}
			return reservation
		}()},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM provider_failure_recovery WHERE failed_task_id=$1`, test.reservation.failedTaskID)
			})
			if _, err := insertProviderFailureReservation(ctx, pool, test.reservation, false); err == nil {
				t.Fatal("invalid reservation shape was accepted")
			} else {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
					t.Fatalf("invalid shape error = %v, want check-constraint violation", err)
				}
			}
		})
	}
}

func TestProviderFailureRecoveryReservationIsIdempotentConcurrently(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	reservation := newProviderFailureReservation("recovery_reserved")
	reservation.taskID = dbid.NewV7()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM provider_failure_recovery WHERE failed_task_id=$1`, reservation.failedTaskID)
	})

	start := make(chan struct{})
	results := make(chan int64, 2)
	errorsFound := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			rows, err := insertProviderFailureReservation(ctx, pool, reservation, true)
			results <- rows
			errorsFound <- err
		}()
	}
	close(start)
	inserted := int64(0)
	for range 2 {
		if err := <-errorsFound; err != nil {
			t.Fatal(err)
		}
		inserted += <-results
	}
	if inserted != 1 {
		t.Fatalf("concurrent reservation inserted %d rows, want exactly one", inserted)
	}
}

func TestProviderFailureRecoveryAllowsOnlyOneRearmPerWakeup(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	wakeupID := dbid.NewV7()
	first := newProviderFailureReservation("recovery_reserved")
	first.wakeupID = wakeupID
	first.wakeupRev = pgtype.Int8{Int64: 2, Valid: true}
	second := newProviderFailureReservation("recovery_reserved")
	second.wakeupID = wakeupID
	second.wakeupRev = pgtype.Int8{Int64: 3, Valid: true}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM provider_failure_recovery WHERE failed_task_id IN ($1,$2)`, first.failedTaskID, second.failedTaskID)
	})
	if _, err := insertProviderFailureReservation(ctx, pool, first, false); err != nil {
		t.Fatalf("first rearm reservation: %v", err)
	}
	if _, err := insertProviderFailureReservation(ctx, pool, second, false); err == nil {
		t.Fatal("second rearm reservation for the same wakeup was accepted")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("second rearm error = %v, want unique violation", err)
		}
	}
}

func TestProviderFailureRecoveryReservationRollsBackWithFailedApply(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	reservation := newProviderFailureReservation("recovery_reserved")
	reservation.taskID = dbid.NewV7()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM provider_failure_recovery WHERE failed_task_id=$1`, reservation.failedTaskID)
	})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE recovery_apply_test (value text NOT NULL CHECK (value <> 'fail')) ON COMMIT DROP`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	applied := false
	if inserted, err := reserveProviderFailureRecoveryAndApply(ctx, tx, providerFailureReservationParams(reservation), func(ctx context.Context, tx pgx.Tx) error {
		applied = true
		_, err := tx.Exec(ctx, `INSERT INTO recovery_apply_test(value) VALUES ('fail')`)
		return err
	}); err == nil || inserted || !applied {
		t.Fatalf("failed apply returned inserted=%t, applied=%t, err=%v", inserted, applied, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id=$1`, reservation.failedTaskID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed apply left %d reservation rows", count)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE recovery_apply_test (value text NOT NULL CHECK (value <> 'fail')) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	if inserted, err := reserveProviderFailureRecoveryAndApply(ctx, tx, providerFailureReservationParams(reservation), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO recovery_apply_test(value) VALUES ('ok')`)
		return err
	}); err != nil || !inserted {
		t.Fatalf("later reservation retry inserted=%t, err=%v", inserted, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id=$1`, reservation.failedTaskID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("successful retry left %d reservation rows, want one", count)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	applyCalled := false
	if inserted, err := reserveProviderFailureRecoveryAndApply(ctx, tx, providerFailureReservationParams(reservation), func(context.Context, pgx.Tx) error {
		applyCalled = true
		return nil
	}); err != nil || inserted || applyCalled {
		t.Fatalf("duplicate reservation returned inserted=%t, applyCalled=%t, err=%v", inserted, applyCalled, err)
	}
}

func TestProviderFailureReservationApplyErrorKeepsCallerTransaction(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	reservation := newProviderFailureReservation("recovery_reserved")
	reservation.taskID = dbid.NewV7()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM provider_failure_recovery WHERE failed_task_id=$1`, reservation.failedTaskID)
	})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE recovery_caller_test (value text NOT NULL CHECK (value <> 'fail')) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO recovery_caller_test(value) VALUES ('before')`); err != nil {
		t.Fatal(err)
	}
	inserted, err := reserveProviderFailureRecoveryAndApply(ctx, tx, providerFailureReservationParams(reservation), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO recovery_caller_test(value) VALUES ('fail')`)
		return err
	})
	if err == nil || inserted {
		t.Fatalf("failed apply returned inserted=%t, err=%v", inserted, err)
	}

	var before int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM recovery_caller_test WHERE value='before'`).Scan(&before); err != nil {
		t.Fatalf("caller transaction unusable after failed apply: %v", err)
	}
	if before != 1 {
		t.Fatalf("caller write before the seam survived %d times, want once", before)
	}
	var reserved int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id=$1`, reservation.failedTaskID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 {
		t.Fatalf("failed apply left %d reservation rows inside the caller transaction", reserved)
	}
}

func TestProviderFailureReservationRefusesDuplicateEffectsWithoutAbortingCaller(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()
	wakeupID, runID, taskID := dbid.NewV7(), dbid.NewV7(), dbid.NewV7()
	original := func(mutate func(*providerFailureReservation)) providerFailureReservation {
		r := newProviderFailureReservation("recovery_reserved")
		mutate(&r)
		return r
	}
	firstWakeup := original(func(r *providerFailureReservation) {
		r.wakeupID, r.wakeupRev = wakeupID, pgtype.Int8{Int64: 2, Valid: true}
	})
	firstTask := original(func(r *providerFailureReservation) { r.taskID = taskID })
	probeAt, probeStatus := recoveryProbeEvidence()
	firstRun := original(func(r *providerFailureReservation) {
		r.changed, r.probeAt, r.probeStatus, r.runID = providerFailureChangedState, probeAt, probeStatus, runID
	})
	seeded := []providerFailureReservation{firstWakeup, firstTask, firstRun}
	t.Cleanup(func() {
		for _, r := range seeded {
			_, _ = pool.Exec(context.Background(), `DELETE FROM provider_failure_recovery WHERE failed_task_id=$1`, r.failedTaskID)
		}
	})
	for _, r := range seeded {
		if _, err := insertProviderFailureReservation(ctx, pool, r, false); err != nil {
			t.Fatalf("seed reservation: %v", err)
		}
	}

	cases := map[string]providerFailureReservation{
		"second re-arm for the same wakeup": original(func(r *providerFailureReservation) {
			r.wakeupID, r.wakeupRev = wakeupID, pgtype.Int8{Int64: 3, Valid: true}
		}),
		"duplicate recovery task": original(func(r *providerFailureReservation) { r.taskID = taskID }),
		"duplicate recovery run": original(func(r *providerFailureReservation) {
			r.changed, r.probeAt, r.probeStatus, r.runID = providerFailureChangedState, probeAt, probeStatus, runID
		}),
	}
	for name, duplicate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM provider_failure_recovery WHERE failed_task_id=$1`, duplicate.failedTaskID)
			})
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			applyCalled := false
			inserted, err := reserveProviderFailureRecoveryAndApply(ctx, tx, providerFailureReservationParams(duplicate), func(context.Context, pgx.Tx) error {
				applyCalled = true
				return nil
			})
			if err != nil || inserted || applyCalled {
				t.Fatalf("duplicate effect returned inserted=%t, applyCalled=%t, err=%v", inserted, applyCalled, err)
			}
			var one int
			if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
				t.Fatalf("caller transaction aborted by the refused reservation: %v", err)
			}
		})
	}
}

var (
	failedTaskSetPattern = regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?(?:public\.)?agent_task_queue\b(?:\s+(?:AS\s+)?[a-z_][a-z0-9_$]*)?\s+SET\s+(.*?)(?:\bFROM\b|\bWHERE\b|\bRETURNING\b|;|$)`)
	failedStatusPattern  = regexp.MustCompile(`(?is)\bstatus\s*=\s*'failed'`)
	failureReasonPattern = regexp.MustCompile(`(?is)\bfailure_reason\s*=`)
	fixedReasonPattern   = regexp.MustCompile(`(?im)^\s*failure_reason\s*=\s*'([^']+)'`)
	namedQueryPattern    = regexp.MustCompile(`(?m)^-- name:\s*([A-Za-z0-9_]+)\s+:[A-Za-z]+`)
	goStringPattern      = regexp.MustCompile("(?s)`([^`]*)`|\"((?:\\\\.|[^\"\\\\])*)\"")
)

func hasProviderFailureWriter(sql string) bool {
	for _, match := range failedTaskSetPattern.FindAllStringSubmatch(sql, -1) {
		if failedStatusPattern.MatchString(match[1]) && failureReasonPattern.MatchString(match[1]) {
			return true
		}
	}
	return false
}

func queryFailureWriters(path string, contents []byte) (map[string]string, error) {
	writers := make(map[string]string)
	matches := namedQueryPattern.FindAllSubmatchIndex(contents, -1)
	for index, match := range matches {
		end := len(contents)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		body := string(contents[match[1]:end])
		if !hasProviderFailureWriter(body) {
			continue
		}
		name := string(contents[match[2]:match[3]])
		id := filepath.ToSlash(path) + "#" + name
		if _, exists := writers[id]; exists {
			return nil, fmt.Errorf("duplicate writer %s", id)
		}
		writers[id] = fixedFailureReason(body)
	}
	return writers, nil
}

func fixedFailureReason(sql string) string {
	for _, match := range failedTaskSetPattern.FindAllStringSubmatch(sql, -1) {
		if !failedStatusPattern.MatchString(match[1]) || !failureReasonPattern.MatchString(match[1]) {
			continue
		}
		reason := fixedReasonPattern.FindStringSubmatch(match[1])
		if len(reason) == 2 {
			return reason[1]
		}
	}
	return "dynamic"
}

func collectProviderFailureWriters(serverRoot string) (map[string]string, error) {
	writers := make(map[string]string)
	queryFiles, err := filepath.Glob(filepath.Join(serverRoot, "pkg", "db", "queries", "*.sql"))
	if err != nil {
		return nil, err
	}
	for _, file := range queryFiles {
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		relative, _ := filepath.Rel(serverRoot, file)
		found, err := queryFailureWriters(filepath.ToSlash(relative), contents)
		if err != nil {
			return nil, err
		}
		mergeWriterInventory(writers, found)
	}

	goFiles := make([]string, 0)
	err = filepath.WalkDir(serverRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != serverRoot && strings.Contains(path, string(filepath.Separator)+"generated") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) == ".go" && !strings.HasSuffix(path, "_test.go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, file := range goFiles {
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		relative, _ := filepath.Rel(serverRoot, file)
		for _, match := range goStringPattern.FindAllSubmatch(contents, -1) {
			sql := string(match[1])
			if sql == "" {
				sql = string(match[2])
			}
			if hasProviderFailureWriter(sql) {
				writers[filepath.ToSlash(relative)+"#inline"] = fixedFailureReason(sql)
			}
		}
	}

	migrations, err := filepath.Glob(filepath.Join(serverRoot, "migrations", "*.sql"))
	if err != nil {
		return nil, err
	}
	for _, file := range migrations {
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		relative, _ := filepath.Rel(serverRoot, file)
		for _, statement := range strings.Split(string(contents), ";") {
			if hasProviderFailureWriter(statement) {
				writers[filepath.ToSlash(relative)+"#migration"] = fixedFailureReason(statement)
			}
		}
	}
	return writers, nil
}

func mergeWriterInventory(dst, src map[string]string) {
	for id, reason := range src {
		dst[id] = reason
	}
}

func validateProviderFailureWriterInventory(actual, expected map[string]string) error {
	ids := make([]string, 0, len(actual))
	for id := range actual {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		want, ok := expected[id]
		if !ok {
			return fmt.Errorf("unclassified failed-task writer %s with reason %q", id, actual[id])
		}
		if actual[id] != want {
			return fmt.Errorf("failed-task writer %s reason = %q, want %q", id, actual[id], want)
		}
	}
	for id := range expected {
		if _, ok := actual[id]; !ok {
			return fmt.Errorf("expected failed-task writer %s was not found", id)
		}
	}
	return nil
}

func TestProviderFailureRecoveryWriterInventory(t *testing.T) {
	serverRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := collectProviderFailureWriters(serverRoot)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"pkg/db/queries/agent.sql#FailAgentTask":                      "dynamic",
		"pkg/db/queries/agent.sql#RecoverOrphanedTasksForRuntime":     "runtime_recovery",
		"pkg/db/queries/agent.sql#FailStaleTasks":                     "timeout",
		"pkg/db/queries/agent.sql#ExpireStaleQueuedTasks":             "queued_expired",
		"pkg/db/queries/agent.sql#FailExpiredRuntimeReconnectRetries": "runtime_reconnect_timeout",
		"pkg/db/queries/runtime.sql#FailTasksForOfflineRuntimes":      "runtime_offline",
	}
	if err := validateProviderFailureWriterInventory(actual, expected); err != nil {
		t.Fatal(err)
	}
}

func TestProviderFailureRecoveryWriterInventoryRejectsFakeUnclassifiedWriter(t *testing.T) {
	fake, err := queryFailureWriters("pkg/db/queries/new.sql", []byte(`-- name: FailNewTask :exec
UPDATE agent_task_queue
SET status = 'failed',
    failure_reason = 'new_failure_kind';
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fake) != 1 {
		t.Fatalf("fake writer scan found %d entries, want one", len(fake))
	}
	actual := map[string]string{
		"pkg/db/queries/agent.sql#FailAgentTask":                      "dynamic",
		"pkg/db/queries/agent.sql#RecoverOrphanedTasksForRuntime":     "runtime_recovery",
		"pkg/db/queries/agent.sql#FailStaleTasks":                     "timeout",
		"pkg/db/queries/agent.sql#ExpireStaleQueuedTasks":             "queued_expired",
		"pkg/db/queries/agent.sql#FailExpiredRuntimeReconnectRetries": "runtime_reconnect_timeout",
		"pkg/db/queries/runtime.sql#FailTasksForOfflineRuntimes":      "runtime_offline",
	}
	mergeWriterInventory(actual, fake)
	expected := map[string]string{
		"pkg/db/queries/agent.sql#FailAgentTask":                      "dynamic",
		"pkg/db/queries/agent.sql#RecoverOrphanedTasksForRuntime":     "runtime_recovery",
		"pkg/db/queries/agent.sql#FailStaleTasks":                     "timeout",
		"pkg/db/queries/agent.sql#ExpireStaleQueuedTasks":             "queued_expired",
		"pkg/db/queries/agent.sql#FailExpiredRuntimeReconnectRetries": "runtime_reconnect_timeout",
		"pkg/db/queries/runtime.sql#FailTasksForOfflineRuntimes":      "runtime_offline",
	}
	if err := validateProviderFailureWriterInventory(actual, expected); err == nil || !strings.Contains(err.Error(), "unclassified") {
		t.Fatalf("fake writer validation error = %v, want unclassified writer", err)
	}
}
