package handler

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The pr_merge_owed row (CHE-5243): a write-ahead intent that an issue's PR
// merge still owes a completion decision. Completion is fenced by the row's
// (id, revision): only the generation it read before deciding may delete or
// reschedule the row.

type owedRow struct {
	ID       string
	Revision int64
	Fire     int32
	LastErr  *string
	NextFire time.Time
}

func owedRowFor(t *testing.T, issueID string) *owedRow {
	t.Helper()
	var r owedRow
	err := testPool.QueryRow(context.Background(),
		`SELECT id::text, revision, fire_count, last_error, next_fire_at FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issueID).
		Scan(&r.ID, &r.Revision, &r.Fire, &r.LastErr, &r.NextFire)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil
		}
		t.Fatalf("read owed row: %v", err)
	}
	return &r
}

func mustOwed(t *testing.T, issueID string) *owedRow {
	t.Helper()
	r := owedRowFor(t, issueID)
	if r == nil {
		t.Fatal("expected a pr_merge_owed row, found none")
	}
	return r
}

func owedIssue(t *testing.T, title string) string {
	t.Helper()
	return prAutoCompleteTestIssue(t, title, 0).ID
}

func upsertOwed(t *testing.T, issueID string) {
	t.Helper()
	if err := upsertPRMergeOwed(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(issueID)); err != nil {
		t.Fatalf("upsertPRMergeOwed: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issueID)
	})
}

func fixtureOwed(t *testing.T) {
	t.Helper()
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
}

func TestPRMergeOwed_MigrationAcceptsRuleAndDownDropsIt(t *testing.T) {
	fixtureOwed(t)
	ctx := context.Background()
	up, err := os.ReadFile("../../migrations/999d_pr_merge_owed_wakeup.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../migrations/999d_pr_merge_owed_wakeup.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	issue := owedIssue(t, "owed migration")
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	insert := `INSERT INTO issue_wakeup(id, workspace_id, issue_id, instruction, kind, mode, event_types, timezone, condition_state, enabled, system_rule, next_fire_at)
	 VALUES (gen_random_uuid(), $1, $2, '', 'at', 'once', '{}', 'UTC', '', true, 'pr_merge_owed', now())`
	if _, err := tx.Exec(ctx, string(up)); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := tx.Exec(ctx, insert, testWorkspaceID, issue); err != nil {
		t.Fatalf("up must accept pr_merge_owed: %v", err)
	}
	if _, err := tx.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issue).Scan(&n); err != nil || n != 0 {
		t.Fatalf("down must delete the rule's rows: n=%d err=%v", n, err)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, insert, testWorkspaceID, issue); err == nil {
		t.Fatal("down must restore the CHECK that refuses pr_merge_owed")
	}
}

func TestPRMergeOwed_UpsertCreatesThenBumpsRevisionKeepingID(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed upsert")
	before := time.Now()
	upsertOwed(t, issue)
	first := owedRowFor(t, issue)
	if first == nil || first.Revision != 1 {
		t.Fatalf("first upsert: %+v", first)
	}
	if d := first.NextFire.Sub(before); d < time.Minute || d > 3*time.Minute {
		t.Fatalf("due should be about 2m out, got %v", d)
	}
	upsertOwed(t, issue)
	second := mustOwed(t, issue)
	if second.ID != first.ID || second.Revision != 2 {
		t.Fatalf("second upsert must keep id and bump revision: first=%+v second=%+v", first, second)
	}
}

func TestPRMergeOwed_UpsertResetsRetryStateOfAnOlderGeneration(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed reset")
	upsertOwed(t, issue)
	testPool.Exec(context.Background(), `UPDATE issue_wakeup SET fire_count=4, last_error='boom' WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issue)
	upsertOwed(t, issue)
	r := mustOwed(t, issue)
	if r.Fire != 0 || r.LastErr != nil {
		t.Fatalf("a new generation starts a fresh retry history: %+v", r)
	}
}

func TestPRMergeOwed_UpsertSkippedWhenWorkspaceMergeStatusIsNone(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed none")
	ctx := context.Background()
	var old []byte
	if err := testPool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(old))
	})
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings,'{}'::jsonb) || '{"pr_merge_status":"none"}'::jsonb WHERE id=$1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	upsertOwed(t, issue)
	if r := owedRowFor(t, issue); r != nil {
		t.Fatalf("no row when the workspace merges nothing: %+v", r)
	}
}

func TestPRMergeOwed_SuccessDeletesOwnGeneration(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed success")
	upsertOwed(t, issue)
	// A state that moves nothing (no merged PR) is still a settled decision.
	if err := testHandler.runPRMergeOwed(context.Background(), parseUUID(testWorkspaceID), parseUUID(issue), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if r := owedRowFor(t, issue); r != nil {
		t.Fatalf("settled attempt must delete its generation: %+v", r)
	}
}

func TestPRMergeOwed_FailureReschedulesWithBackoffAndKeepsRevision(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed failure")
	upsertOwed(t, issue)
	before := mustOwed(t, issue)
	boom := errors.New("boom")
	start := time.Now()
	if err := testHandler.runPRMergeOwed(context.Background(), parseUUID(testWorkspaceID), parseUUID(issue), func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("the decision's error is returned, got %v", err)
	}
	after := owedRowFor(t, issue)
	if after == nil || after.Revision != before.Revision || after.ID != before.ID {
		t.Fatalf("a failure never changes the generation: before=%+v after=%+v", before, after)
	}
	if after.LastErr == nil || !strings.Contains(*after.LastErr, "boom") {
		t.Fatalf("last_error must record the failure: %+v", after)
	}
	if d := after.NextFire.Sub(start); d < 20*time.Second || d > 45*time.Second {
		t.Fatalf("first retry is about 30s out, got %v", d)
	}
	if after.Fire != 1 {
		t.Fatalf("fire_count counts failed attempts, got %d", after.Fire)
	}
}

func TestPRMergeOwed_BackoffDoublesFrom30sAndCapsAtOneHour(t *testing.T) {
	want := map[int32]time.Duration{0: 30 * time.Second, 1: time.Minute, 2: 2 * time.Minute, 6: 32 * time.Minute, 7: time.Hour, 50: time.Hour, 1 << 30: time.Hour}
	for n, d := range want {
		if got := prMergeOwedBackoff(n); got != d {
			t.Errorf("backoff(%d) = %v, want %v", n, got, d)
		}
	}
}

// A revision bump between the generation read and the delete (a newer merge
// event arrived while this attempt decided) must keep the row.
func TestPRMergeOwed_BumpDuringDecisionKeepsRowAndRevision(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed interleave")
	upsertOwed(t, issue)
	err := testHandler.runPRMergeOwed(context.Background(), parseUUID(testWorkspaceID), parseUUID(issue), func(ctx context.Context) error {
		return upsertPRMergeOwed(ctx, testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(issue))
	})
	if err != nil {
		t.Fatal(err)
	}
	r := owedRowFor(t, issue)
	if r == nil || r.Revision != 2 {
		t.Fatalf("stale attempt must not delete the newer generation: %+v", r)
	}
}

// The delete waits behind another transaction's uncommitted bump. After that
// commits, READ COMMITTED re-checks the predicate and the delete matches nothing.
func TestPRMergeOwed_DeleteWaitingOnBumpLockMatchesNothingAfterCommit(t *testing.T) {
	fixtureOwed(t)
	ctx := context.Background()
	issue := owedIssue(t, "owed lock wait")
	upsertOwed(t, issue)
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	done := make(chan error, 1)
	err = testHandler.runPRMergeOwed(ctx, parseUUID(testWorkspaceID), parseUUID(issue), func(ctx context.Context) error {
		// Hold the row lock with an uncommitted bump, then let the attempt's
		// delete run into it from a goroutine-free path: release after a
		// waiter is seen on the lock.
		if err := upsertPRMergeOwed(ctx, db.New(tx), parseUUID(testWorkspaceID), parseUUID(issue)); err != nil {
			return err
		}
		go func() {
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				var waiting int
				testPool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query ILIKE '%DELETE FROM issue_wakeup%'`).Scan(&waiting)
				if waiting > 0 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			done <- tx.Commit(ctx)
		}()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("commit: %v", err)
	}
	if r := owedRowFor(t, issue); r == nil || r.Revision != 2 {
		t.Fatalf("delete that waited on the bump must match nothing: %+v", r)
	}
}

// No row at read time means no generation: nothing is deleted or rescheduled,
// even if a row appears while the attempt decides.
func TestPRMergeOwed_AbsentRowAtReadNeverDeletesALaterRow(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed absent")
	if err := testHandler.runPRMergeOwed(context.Background(), parseUUID(testWorkspaceID), parseUUID(issue), func(ctx context.Context) error {
		return upsertPRMergeOwed(ctx, testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(issue))
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issue)
	})
	if r := owedRowFor(t, issue); r == nil || r.Revision != 1 {
		t.Fatalf("row created mid-attempt must survive: %+v", r)
	}
}

// A row deleted and recreated mid-attempt has a new id, so the old (id,
// revision) pair matches nothing even when the revision numbers agree.
func TestPRMergeOwed_RecreatedRowIsNotDeletedByOlderAttempt(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed recreate")
	upsertOwed(t, issue)
	old := mustOwed(t, issue)
	if err := testHandler.runPRMergeOwed(context.Background(), parseUUID(testWorkspaceID), parseUUID(issue), func(ctx context.Context) error {
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issue)
		return upsertPRMergeOwed(ctx, testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(issue))
	}); err != nil {
		t.Fatal(err)
	}
	r := owedRowFor(t, issue)
	if r == nil || r.ID == old.ID || r.Revision != 1 {
		t.Fatalf("recreated generation must survive: old=%+v new=%+v", old, r)
	}
}

// A failing attempt whose generation was bumped meanwhile must not push the
// newer generation's due time out or write its error.
func TestPRMergeOwed_StaleFailureDoesNotRescheduleNewerGeneration(t *testing.T) {
	fixtureOwed(t)
	issue := owedIssue(t, "owed stale reschedule")
	upsertOwed(t, issue)
	boom := errors.New("boom")
	if err := testHandler.runPRMergeOwed(context.Background(), parseUUID(testWorkspaceID), parseUUID(issue), func(ctx context.Context) error {
		if err := upsertPRMergeOwed(ctx, testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(issue)); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	r := owedRowFor(t, issue)
	if r == nil || r.Revision != 2 || r.LastErr != nil || r.Fire != 0 {
		t.Fatalf("stale failure must leave the newer generation alone: %+v", r)
	}
	if d := time.Until(r.NextFire); d < time.Minute {
		t.Fatalf("newer generation keeps its own due time (~2m), got %v", d)
	}
}

func TestPRMergeOwed_EvaluateCompletesIssueWithMergedPRAndDeletesRow(t *testing.T) {
	fixtureOwed(t)
	secret := "owed-merged"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	const inst int64 = 5243001
	issue := prAutoCompleteTestIssue(t, "owed evaluate", inst)
	firePRWebhook(t, secret, inst, 1, "Closes "+issue.Identifier, "", "fix/a", "merged")
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='in_progress' WHERE id=$1`, issue.ID); err != nil {
		t.Fatal(err)
	}
	upsertOwed(t, issue.ID)
	w, err := testHandler.Queries.GetIssueWakeup(context.Background(), db.GetIssueWakeupParams{ID: parseUUID(mustOwed(t, issue.ID).ID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := testHandler.evaluatePRMergeOwed(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if got := issueStatusForTest(t, issue.ID); got != "done" {
		t.Fatalf("merged PR must complete the issue, got %s", got)
	}
	if r := owedRowFor(t, issue.ID); r != nil {
		t.Fatalf("row must be gone after the move: %+v", r)
	}
}

// The scheduler routes a due pr_merge_owed row to the handler hook and never
// lets the generic id-only failure note overwrite the fenced last_error.
func TestPRMergeOwed_SchedulerRoutesDueRowToHook(t *testing.T) {
	fixtureOwed(t)
	ctx := context.Background()
	issue := owedIssue(t, "owed scheduler")
	upsertOwed(t, issue)
	testPool.Exec(ctx, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 minute' WHERE issue_id=$1 AND system_rule='pr_merge_owed'`, issue)
	original := testHandler.TaskService.PRMergeOwed
	t.Cleanup(func() { testHandler.TaskService.PRMergeOwed = original })
	calls := 0
	testHandler.TaskService.PRMergeOwed = func(_ context.Context, w db.IssueWakeup) error {
		calls++
		return errors.New("hook failed")
	}
	_ = (&service.IssueWakeupService{Tasks: testHandler.TaskService}).TickWorkspaces(ctx, parseUUID(testWorkspaceID))
	if calls != 1 {
		t.Fatalf("hook calls = %d, want 1", calls)
	}
	if r := owedRowFor(t, issue); r == nil || r.LastErr != nil {
		t.Fatalf("the hook owns last_error for this rule: %+v", r)
	}
}
