package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// activationFixture is a wakeup fixture whose activation runs under its own key,
// so no other test sees its state and the shared database keeps no row behind.
func activationFixture(t *testing.T) (principalFixture, *IssueWakeupService, pgtype.UUID) {
	t.Helper()
	f, s, issue, agent := wakeFixture(t)
	s.ActivationKey = "test_" + f.WorkspaceID
	f.Cleanup(t, "DELETE FROM wakeup_definition_activation WHERE name=$1", s.ActivationKey)
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	assignPRWakeupIssue(t, f, issue, agent)
	return f, s, issue
}

// workCounts is every kind of queued work a definition could cause.
func workCounts(t *testing.T, f principalFixture) [4]int {
	t.Helper()
	return [4]int{
		f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id=$1)`, f.WorkspaceID),
		f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE workspace_id=$1)`, f.WorkspaceID),
		f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE workspace_id=$1`, f.WorkspaceID),
		f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1`, f.WorkspaceID),
	}
}

func mustActivated(t *testing.T, s *IssueWakeupService) bool {
	t.Helper()
	ok, err := s.WakeupDefinitionsActivated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A fresh database has no activation row: the gate follows it, so it is closed,
// and reading the state creates nothing.
func TestActivationIsClosedAndEmptyBeforeItRuns(t *testing.T) {
	f, s, _ := activationFixture(t)
	if mustActivated(t, s) {
		t.Fatal("definition writes are open before activation ran")
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID); n != 0 {
		t.Fatalf("%d definitions before activation, want zero", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM wakeup_definition_activation WHERE name=$1`, s.ActivationKey); n != 0 {
		t.Fatalf("reading the gate created %d activation rows", n)
	}
}

// Activation backfills, verifies, then opens the gate, and queues nothing.
func TestActivationBackfillsVerifiesAndOpensTheGateWithoutQueuingWork(t *testing.T) {
	f, s, issue := activationFixture(t)
	ctx := context.Background()
	off, text := false, "custom"
	customizeChildDone(t, s, issue, &off, &text)
	before := workCounts(t, f)

	res, err := s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 50, MaxPages: 10, WorkspaceID: parseTestUUID(t, f.WorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || res.Inserted != 1 || !mustActivated(t, s) {
		t.Fatalf("result = %+v activated=%v, want complete with one definition", res, mustActivated(t, s))
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID); n != 1 {
		t.Fatalf("definitions = %d, want 1", n)
	}
	if after := workCounts(t, f); after != before {
		t.Fatalf("activation queued work: %v -> %v", before, after)
	}
	// Running again is a no-op that scans nothing.
	again, err := s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 50, MaxPages: 10, WorkspaceID: parseTestUUID(t, f.WorkspaceID)})
	if err != nil || !again.Complete || again.Scanned != 0 || again.Inserted != 0 {
		t.Fatalf("second run = %+v %v", again, err)
	}
}

// An interrupted run keeps the gate closed, resumes from its persisted cursor,
// and never repeats or duplicates a finished page.
func TestActivationInterruptionResumesFromThePersistedCursor(t *testing.T) {
	f, s, issue := activationFixture(t)
	ctx := context.Background()
	ws := parseTestUUID(t, f.WorkspaceID)
	off := false
	customizeChildDone(t, s, issue, &off, nil)
	second := parseTestUUID(t, f.Issue(t, "second parent"))
	f.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", second)
	text := "second text"
	customizeChildDone(t, s, second, nil, &text)

	part, err := s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 1, MaxPages: 1, WorkspaceID: ws})
	if err != nil {
		t.Fatal(err)
	}
	if part.Complete || part.Inserted != 1 || mustActivated(t, s) {
		t.Fatalf("interrupted run = %+v activated=%v, want one page and a closed gate", part, mustActivated(t, s))
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID); n != 1 {
		t.Fatalf("definitions after the interruption = %d, want 1", n)
	}

	var final ActivationResult
	for i := 0; i < 10 && !final.Complete; i++ {
		if final, err = s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 1, MaxPages: 1, WorkspaceID: ws}); err != nil {
			t.Fatal(err)
		}
	}
	if !final.Complete || !mustActivated(t, s) {
		t.Fatalf("retry never completed: %+v", final)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID); n != 2 {
		t.Fatalf("definitions after the retry = %d, want 2 (no duplicates)", n)
	}
}

// A cancelled run leaves the persisted state intact for the next one.
func TestActivationCancelledRunLeavesTheGateClosed(t *testing.T) {
	f, s, issue := activationFixture(t)
	off := false
	customizeChildDone(t, s, issue, &off, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 10, MaxPages: 10, WorkspaceID: parseTestUUID(t, f.WorkspaceID)}); err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if mustActivated(t, s) {
		t.Fatal("a cancelled run opened the gate")
	}
}

// The gate stays closed when verification finds something other than a
// backfilled issue override, and the failure is recorded, not hidden. Once it
// is gone the next run opens the gate.
func TestActivationStaysClosedWhenVerificationFails(t *testing.T) {
	f, s, issue := activationFixture(t)
	ctx := context.Background()
	ws := parseTestUUID(t, f.WorkspaceID)
	off := false
	customizeChildDone(t, s, issue, &off, nil)
	// A custom root rule exists before activation: production must hold none.
	stray := "6f1a2b3c-4d5e-4f60-8a7b-9c0d1e2f3a4b"
	if err := insertDefinition(t, f, "workspace", f.WorkspaceID, stray, true, `{"v":1,"enabled":true}`); err != nil {
		t.Fatal(err)
	}

	res, err := s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 50, MaxPages: 10, WorkspaceID: ws})
	if !errors.Is(err, ErrActivationVerification) {
		t.Fatalf("err = %v, want ErrActivationVerification", err)
	}
	if res.Complete || mustActivated(t, s) {
		t.Fatalf("verification failure opened the gate: %+v", res)
	}
	var lastError string
	if err := f.Pool.QueryRow(ctx, `SELECT coalesce(last_error,'') FROM wakeup_definition_activation WHERE name=$1`, s.ActivationKey).Scan(&lastError); err != nil || lastError == "" {
		t.Fatalf("failure was not recorded: %q %v", lastError, err)
	}

	f.Exec(t, `DELETE FROM issue_wakeup_definition WHERE workspace_id=$1 AND rule_key=$2`, f.WorkspaceID, stray)
	res, err = s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 50, MaxPages: 10, WorkspaceID: ws})
	if err != nil || !res.Complete || !mustActivated(t, s) {
		t.Fatalf("retry after the fix = %+v %v", res, err)
	}
}

// Only the verified backfill may open the gate: a row left running by an earlier
// pass is not activation.
func TestActivationRunningRowDoesNotOpenTheGate(t *testing.T) {
	f, s, _ := activationFixture(t)
	f.Exec(t, `INSERT INTO wakeup_definition_activation(name, state) VALUES($1, 'backfill')`, s.ActivationKey)
	if mustActivated(t, s) {
		t.Fatal("a running activation opened the gate")
	}
}

// The scheduler job must not hide a failed activation: the error surfaces so the
// next tick retries and the failure is logged.
func TestActivationRequiresPositivePage(t *testing.T) {
	_, s, _ := activationFixture(t)
	if _, err := s.ActivateWakeupDefinitions(context.Background(), ActivationRequest{}); !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("zero page = %v, want ErrWakeupInput", err)
	}
}
