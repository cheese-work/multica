package receipt

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/governance"
)

// These tests exercise the receipt evaluation path against a real
// *governance.BudgetService (not the in-memory fakeBudgetAdmission the rest of
// this file's tests use). A background context keeps database timing separate
// from the production observation deadline tested in receipt_test.go.
// CHE-707 review B1 was invisible to a fake BudgetAdmission — the fake always
// admits, regardless of what AttemptID/ReservationID it was called with — so
// only a real BudgetService reproduces the bug: the create and every later
// edit on the same comment collapsed onto the identical ReservationID
// (bare CommentID) and only the first observation was ever actually admitted.

type receiptBudgetFixture struct {
	pool        *pgxpool.Pool
	workspaceID pgtype.UUID
	service     *governance.BudgetService
}

func evaluateReceiptWithoutLatencyBudget(observer *Observer, input Input) Result {
	result, done := observer.evaluate(context.Background(), input)
	<-done
	return result
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func newReceiptBudgetFixture(t *testing.T) *receiptBudgetFixture {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	workspaceID := receiptTestUUID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO governance_workspace_config (workspace_id, control_epoch, settings)
		VALUES ($1, 1, '{"jev_governance_enabled":true,"rule_mode":"shadow"}')
	`, workspaceID); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{
			"governance_budget_outbox",
			"governance_budget_journal",
			"governance_budget_reservation",
			"governance_budget_root",
			"governance_budget_window",
			"governance_concurrency_hold",
			"governance_concurrency_guard",
			"governance_workspace_config",
		} {
			if _, err := pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id = $1", workspaceID); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
		pool.Close()
	})
	service, err := governance.NewBudgetService(pool, fixedClock{now: time.Date(2030, time.January, 2, 12, 5, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return &receiptBudgetFixture{pool: pool, workspaceID: workspaceID, service: service}
}

func receiptTestUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

func receiptTestBudgetPolicy() *governance.DeploymentBudgetPolicy {
	return &governance.DeploymentBudgetPolicy{
		Version: "test-v1", Provider: "jev", Model: "test-model",
		InputMicroUSDPerMillionTokens: 100, OutputMicroUSDPerMillionTokens: 200,
		MaxAttemptCostMicroUSD: 60, MaxWindowSpendMicroUSD: 600, MaxWorkspaceSpendMicroUSD: 6000,
		MaxWindowSeconds: 3600, MaxEvaluationsPerCase: 10, MaxConcurrentEvaluations: 10,
	}
}

func receiptTestOperatingLimits() governance.OperatingLimits {
	maxEvaluations := int64(10)
	windowSeconds := int64(3600)
	spendCap := int64(600)
	return governance.OperatingLimits{
		MaxEvaluations:            &maxEvaluations,
		AdmissionWindowSeconds:    &windowSeconds,
		WorkspaceSpendCapMicroUSD: &spendCap,
		PricingPolicy:             &governance.PricingPolicy{Version: "test-v1"},
	}
}

// TestObserve_EditAfterSettledCreateIsAdmittedAgainstRealBudgetService is the
// CHE-707 review B1 regression: before the fix, ReserveEvaluation derived
// ReservationID as the bare CommentID, so a create's reservation and every
// later edit's reservation on the SAME comment were the identical row. Once
// the create settled, the edit's ReserveEvaluation call found that settled
// row under the same ReservationID and (request digest differing, since the
// edit runs in a later admission window) failed with ErrBudgetConflict —
// never reaching the provider at all, regardless of max_evaluations headroom.
//
// This test drives two full receipt evaluations for the same CommentID with
// different (Trigger, CommentRevision) — exactly what CreateComment then
// UpdateComment produce — against a real BudgetService, and asserts the
// second (edit) observation is admitted and decided, not treated as an error.
func TestObserve_EditAfterSettledCreateIsAdmittedAgainstRealBudgetService(t *testing.T) {
	fixture := newReceiptBudgetFixture(t)
	policy := receiptTestBudgetPolicy()
	commentID := receiptTestUUID()
	issueID := receiptTestUUID()

	observer := &Observer{
		Provider:        &fakeProvider{resp: mentionOwnerResponse(0.99)},
		Store:           &fakeStore{},
		BudgetAdmission: fixture.service,
		BudgetPolicy:    policy,
	}

	createInput := Input{
		WorkspaceID:     fixture.workspaceID,
		ControlEpoch:    1,
		Limits:          receiptTestOperatingLimits(),
		IssueID:         issueID,
		CommentID:       commentID,
		CommentRevision: 1,
		Trigger:         TriggerCreate,
		Eval:            oneCandidateOneSpanInput(),
	}
	createResult := evaluateReceiptWithoutLatencyBudget(observer, createInput)
	if createResult.Status != "decided" {
		t.Fatalf("create observation = %+v, want status decided", createResult)
	}
	observer.WaitForIdle()

	editInput := createInput
	editInput.CommentRevision = 2
	editInput.Trigger = TriggerEdit
	editResult := evaluateReceiptWithoutLatencyBudget(observer, editInput)
	if editResult.Status != "decided" {
		t.Fatalf("edit observation after settled create = %+v, want status decided (this is CHE-707 review B1: edits must not reuse the create's reservation identity)", editResult)
	}
	observer.WaitForIdle()

	var reservationCount int64
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1
	`, fixture.workspaceID).Scan(&reservationCount); err != nil {
		t.Fatal(err)
	}
	if reservationCount != 2 {
		t.Fatalf("reservation rows after create+edit = %d, want 2 (distinct attempt identities)", reservationCount)
	}
}

// TestObserve_SecondEditWithoutRevisionChangeReusesReservation guards the
// attemptID derivation's other edge: two Observe calls for the same
// CommentID, Trigger AND CommentRevision (e.g. a caller that genuinely could
// not supply a revision) still resolve deterministically to the same
// ReservationID, so the pre-existing idempotent-retry behavior
// (TestReserveEvaluationIsIdempotentForSamePolicyAndAttempt in the governance
// package) is preserved rather than accidentally defeated by this fix.
func TestObserve_SecondEditWithoutRevisionChangeReusesReservation(t *testing.T) {
	fixture := newReceiptBudgetFixture(t)
	policy := receiptTestBudgetPolicy()
	commentID := receiptTestUUID()
	issueID := receiptTestUUID()

	observer := &Observer{
		Provider:        &fakeProvider{resp: mentionOwnerResponse(0.99)},
		Store:           &fakeStore{},
		BudgetAdmission: fixture.service,
		BudgetPolicy:    policy,
	}
	input := Input{
		WorkspaceID:  fixture.workspaceID,
		ControlEpoch: 1,
		Limits:       receiptTestOperatingLimits(),
		IssueID:      issueID,
		CommentID:    commentID,
		Trigger:      TriggerCreate,
		Eval:         oneCandidateOneSpanInput(),
	}

	first := evaluateReceiptWithoutLatencyBudget(observer, input)
	if first.Status != "decided" {
		t.Fatalf("first observation = %+v, want status decided", first)
	}
	observer.WaitForIdle()

	second := evaluateReceiptWithoutLatencyBudget(observer, input)
	observer.WaitForIdle()
	if second.Status != "error" || second.ShedReason != ReasonError {
		t.Fatalf("second observation with identical (comment, trigger, revision) = %+v, want error/duplicate (unchanged pre-existing idempotency behavior)", second)
	}

	var reservationCount int64
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1
	`, fixture.workspaceID).Scan(&reservationCount); err != nil {
		t.Fatal(err)
	}
	if reservationCount != 1 {
		t.Fatalf("reservation rows for identical (comment, trigger, revision) pair = %d, want 1", reservationCount)
	}
}
