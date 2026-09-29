package governance

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestParseDeploymentBudgetPolicyFailsClosedWhenMissing(t *testing.T) {
	policy, err := ParseDeploymentBudgetPolicy("  ")
	if err != nil || policy != nil {
		t.Fatalf("missing policy = %+v, %v; want nil without error", policy, err)
	}
}

func TestParseDeploymentBudgetPolicyRequiresVersionedPositiveCaps(t *testing.T) {
	for _, raw := range []string{
		`{"version":"v1","provider":"jev","model":"model","input_micro_usd_per_million_tokens":1,"output_micro_usd_per_million_tokens":0,"max_attempt_cost_micro_usd":0,"max_window_spend_micro_usd":1,"max_workspace_spend_micro_usd":1,"max_window_seconds":3600,"max_evaluations_per_case":10,"max_concurrent_evaluations":5}`,
		`{"version":"v1","provider":"jev","model":"model","input_micro_usd_per_million_tokens":0,"output_micro_usd_per_million_tokens":0,"max_attempt_cost_micro_usd":1,"max_window_spend_micro_usd":1,"max_workspace_spend_micro_usd":1,"max_window_seconds":3600,"max_evaluations_per_case":10,"max_concurrent_evaluations":5}`,
		`{"version":"v1","provider":"jev","model":"model","input_micro_usd_per_million_tokens":1,"output_micro_usd_per_million_tokens":0,"max_attempt_cost_micro_usd":2,"max_window_spend_micro_usd":1,"max_workspace_spend_micro_usd":1,"max_window_seconds":3600,"max_evaluations_per_case":10,"max_concurrent_evaluations":5}`,
		`{"version":"v1","provider":"jev","model":"model","input_micro_usd_per_million_tokens":1,"output_micro_usd_per_million_tokens":0,"max_attempt_cost_micro_usd":1,"max_window_spend_micro_usd":2,"max_workspace_spend_micro_usd":1,"max_window_seconds":3600,"max_evaluations_per_case":10,"max_concurrent_evaluations":5}`,
		`{"version":"v1","provider":"jev","model":"model","input_micro_usd_per_million_tokens":1,"output_micro_usd_per_million_tokens":0,"max_attempt_cost_micro_usd":1,"max_window_spend_micro_usd":1,"max_workspace_spend_micro_usd":1,"max_window_seconds":3600,"max_evaluations_per_case":10,"max_concurrent_evaluations":5,"unexpected":true}`,
		`{"version":"v1","provider":"jev","model":"model","input_micro_usd_per_million_tokens":1,"output_micro_usd_per_million_tokens":0,"max_attempt_cost_micro_usd":1,"max_window_spend_micro_usd":1,"max_workspace_spend_micro_usd":1,"max_window_seconds":3600,"max_evaluations_per_case":10,"max_concurrent_evaluations":0}`,
	} {
		if policy, err := ParseDeploymentBudgetPolicy(raw); err == nil || policy != nil {
			t.Fatalf("invalid policy accepted: %+v, %v", policy, err)
		}
	}
}

func TestParseDeploymentBudgetPolicyAcceptsVersionedPolicy(t *testing.T) {
	policy, err := ParseDeploymentBudgetPolicy(`{"version":"v1","provider":"jev","model":"model","input_micro_usd_per_million_tokens":1,"output_micro_usd_per_million_tokens":0,"max_attempt_cost_micro_usd":1,"max_window_spend_micro_usd":1,"max_workspace_spend_micro_usd":1,"max_window_seconds":3600,"max_evaluations_per_case":10,"max_concurrent_evaluations":5}`)
	if err != nil || policy == nil || policy.Version != "v1" {
		t.Fatalf("parsed policy = %+v, %v; want version v1", policy, err)
	}
}

func TestParseDeploymentBudgetPolicyRejectsTrailingData(t *testing.T) {
	policy, err := ParseDeploymentBudgetPolicy(`{"version":"v1","provider":"jev","model":"model","input_micro_usd_per_million_tokens":1,"output_micro_usd_per_million_tokens":0,"max_attempt_cost_micro_usd":1,"max_window_spend_micro_usd":1,"max_workspace_spend_micro_usd":1,"max_window_seconds":3600,"max_evaluations_per_case":10,"max_concurrent_evaluations":5} trailing`)
	if err == nil || policy != nil {
		t.Fatalf("trailing data accepted: %+v, %v", policy, err)
	}
}

func TestDeploymentBudgetPolicyUsesIntegerMicroUSD(t *testing.T) {
	policy := DeploymentBudgetPolicy{
		Version:                        "policy-2026-09",
		Provider:                       "jev",
		Model:                          "jev-1.13",
		InputMicroUSDPerMillionTokens:  1_500_001,
		OutputMicroUSDPerMillionTokens: 2_000_000,
		MaxAttemptCostMicroUSD:         10,
		MaxWindowSpendMicroUSD:         100,
		MaxWorkspaceSpendMicroUSD:      200,
		MaxWindowSeconds:               3600,
		MaxEvaluationsPerCase:          10,
		MaxConcurrentEvaluations:       5,
	}
	cost, err := policy.CostMicroUSD(3, 2)
	if err != nil || cost != 9 {
		t.Fatalf("cost = %d, %v; want 9 micro-USD", cost, err)
	}
	if _, err := policy.CostMicroUSD(math.MaxInt64, math.MaxInt64); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow cost error = %v", err)
	}
}

func TestDeploymentBudgetPolicyCapsWorkspaceEvaluationLimit(t *testing.T) {
	policy := testDeploymentBudgetPolicy(40, 100, 100)
	input := testEvaluationBudgetInput(budgetTestUUID(), budgetTestUUID(), budgetTestUUID(), 11, 100, 1)
	if _, err := policy.EffectiveLimits(input.Limits); !errors.Is(err, ErrBudgetPolicyCap) {
		t.Fatalf("evaluation cap error = %v, want ErrBudgetPolicyCap", err)
	}
}

func TestReserveEvaluationFailsClosedWithoutDeploymentPolicy(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	input := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 2, 100, 1)
	_, err := fixture.service.ReserveEvaluation(context.Background(), nil, input)
	if !errors.Is(err, ErrBudgetPolicyUnavailable) {
		t.Fatalf("missing policy error = %v, want ErrBudgetPolicyUnavailable", err)
	}
	assertBudgetNoArtifacts(t, fixture, input.AttemptID)
}

func TestReserveEvaluationRejectsWorkspaceCapAboveDeploymentPolicy(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	input := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 2, 101, 1)
	_, err := fixture.service.ReserveEvaluation(context.Background(), testDeploymentBudgetPolicy(40, 100, 100), input)
	if !errors.Is(err, ErrBudgetPolicyCap) {
		t.Fatalf("over-cap admission error = %v, want ErrBudgetPolicyCap", err)
	}
	assertBudgetNoArtifacts(t, fixture, input.AttemptID)
}

func TestReserveEvaluationRejectsUnpinnedWorkspacePricingVersion(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	input := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 2, 100, 1)
	input.Limits.PricingPolicy.Version = "unpublished-v2"
	_, err := fixture.service.ReserveEvaluation(context.Background(), testDeploymentBudgetPolicy(40, 100, 100), input)
	if !errors.Is(err, ErrBudgetPolicyVersion) {
		t.Fatalf("unrecognized policy version error = %v, want ErrBudgetPolicyVersion", err)
	}
	assertBudgetNoArtifacts(t, fixture, input.AttemptID)
}

func TestReserveEvaluationEnforcesTotalPerCaseEvaluationLimit(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	caseID := budgetTestUUID()
	first := testEvaluationBudgetInput(fixture.workspaceID, caseID, budgetTestUUID(), 1, 100, 1)
	if _, err := fixture.service.ReserveEvaluation(context.Background(), testDeploymentBudgetPolicy(40, 100, 100), first); err != nil {
		t.Fatalf("first evaluation admission: %v", err)
	}
	second := testEvaluationBudgetInput(fixture.workspaceID, caseID, budgetTestUUID(), 1, 100, 1)
	if _, err := fixture.service.ReserveEvaluation(context.Background(), testDeploymentBudgetPolicy(40, 100, 100), second); !errors.Is(err, ErrBudgetLimit) {
		t.Fatalf("second evaluation admission error = %v, want ErrBudgetLimit", err)
	}
	assertBudgetNoArtifacts(t, fixture, second.AttemptID)
}

func TestReserveEvaluationPerCaseLimitIgnoresOtherResources(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	caseID := budgetTestUUID()
	other := fixture.reserveCommand("other-resource", budgetTestTime(12, 0), budgetTestTime(13, 0), 100, 100, 1, 1)
	other.BudgetRootID = fixture.workspaceID
	other.CaseID = caseID
	if _, err := fixture.service.Reserve(context.Background(), other); err != nil {
		t.Fatalf("reserve other resource: %v", err)
	}

	input := testEvaluationBudgetInput(fixture.workspaceID, caseID, budgetTestUUID(), 1, 100, 1)
	if _, err := fixture.service.ReserveEvaluation(context.Background(), testDeploymentBudgetPolicy(40, 100, 100), input); err != nil {
		t.Fatalf("evaluation admission after other resource: %v", err)
	}
}

// TestReserveEvaluationConcurrencyLimitIsIndependentOfPerCaseLimit is the
// CHE-707 review B2 regression. Before the fix, ReserveEvaluation passed
// *input.Limits.MaxEvaluations (a PER-CASE total-attempts cap) as
// BudgetReserveCommand.SlotLimit, which LockConcurrencyResources /
// guards.Reserve enforce as a WORKSPACE-WIDE in-flight concurrency cap on
// the shared "governance-evaluation" resource (see concurrency.go). A
// workspace with a low per-case max_evaluations setting (here: 1) then had
// its cross-case concurrency throttled to that same number, so a second
// case's first-ever evaluation could be shed with ErrConcurrencyLimit purely
// because an unrelated case's evaluation was still reserved — even though
// each case was well within its own per-case limit.
//
// This test reserves one evaluation for case A (consuming its entire
// max_evaluations=1 budget) and, while that reservation is still held
// (unsettled), reserves one evaluation for a DIFFERENT case B with the same
// max_evaluations=1 setting. With MaxConcurrentEvaluations raised above 1 in
// the deployment policy, case B's reservation must succeed: per-case and
// concurrency limits are independent knobs.
func TestReserveEvaluationConcurrencyLimitIsIndependentOfPerCaseLimit(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	policy := testDeploymentBudgetPolicyWithConcurrency(40, 100, 100, 5)

	caseA := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 1, 100, 1)
	if _, err := fixture.service.ReserveEvaluation(context.Background(), policy, caseA); err != nil {
		t.Fatalf("case A admission: %v", err)
	}

	caseB := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 1, 100, 1)
	if _, err := fixture.service.ReserveEvaluation(context.Background(), policy, caseB); err != nil {
		t.Fatalf("case B admission = %v, want success (concurrency limit must not reuse the per-case max_evaluations setting)", err)
	}
}

// TestReserveEvaluationConcurrencyLimitCapsAcrossCases proves
// MaxConcurrentEvaluations still enforces its own workspace-wide cap once
// that cap is actually reached, so the B2 fix does not accidentally disable
// concurrency limiting altogether: a third case must be refused once
// MaxConcurrentEvaluations (2, here) is exhausted by two other cases' still-
// held reservations, even though each case's own max_evaluations=1 setting
// is not itself the limiting factor.
func TestReserveEvaluationConcurrencyLimitCapsAcrossCases(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	policy := testDeploymentBudgetPolicyWithConcurrency(40, 100, 100, 2)

	first := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 1, 100, 1)
	if _, err := fixture.service.ReserveEvaluation(context.Background(), policy, first); err != nil {
		t.Fatalf("first case admission: %v", err)
	}
	second := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 1, 100, 1)
	if _, err := fixture.service.ReserveEvaluation(context.Background(), policy, second); err != nil {
		t.Fatalf("second case admission: %v", err)
	}
	third := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 1, 100, 1)
	if _, err := fixture.service.ReserveEvaluation(context.Background(), policy, third); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("third case admission error = %v, want ErrConcurrencyLimit", err)
	}
}

func TestReserveEvaluationIsIdempotentForSamePolicyAndAttempt(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	policy := testDeploymentBudgetPolicy(40, 100, 100)
	input := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 3, 100, 1)
	first, err := fixture.service.ReserveEvaluation(context.Background(), policy, input)
	if err != nil || first.Duplicate {
		t.Fatalf("first admission = %+v, %v; want new reservation", first, err)
	}
	second, err := fixture.service.ReserveEvaluation(context.Background(), policy, input)
	if err != nil || !second.Duplicate || second.Reservation.ReservationID != first.Reservation.ReservationID {
		t.Fatalf("duplicate admission = %+v, %v; want same reservation marked duplicate", second, err)
	}
	count := budgetCount(t, fixture.pool, `SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1`, fixture.workspaceID)
	if count != 1 {
		t.Fatalf("reservation rows = %d, want 1", count)
	}
	assertBudgetRootTotals(t, fixture, fixture.workspaceID, 40, 0)
}

func TestSettleEvaluationUsesDeploymentRatesAndReleasesUnusedReservation(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	policy := testDeploymentBudgetPolicy(60, 100, 100)
	input := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 3, 100, 1)
	workspaceInputRate := int64(900_000)
	workspaceOutputRate := int64(800_000)
	input.Limits.PricingPolicy.InputMicroUSDPerMillionTokens = &workspaceInputRate
	input.Limits.PricingPolicy.OutputMicroUSDPerMillionTokens = &workspaceOutputRate
	reservation, err := fixture.service.ReserveEvaluation(context.Background(), policy, input)
	if err != nil {
		t.Fatalf("reserve evaluation: %v", err)
	}
	if err := fixture.service.SettleEvaluation(context.Background(), policy, reservation, "receipt-1", 100_000, 25_000, true); err != nil {
		t.Fatalf("settle evaluation: %v", err)
	}
	assertBudgetRootTotals(t, fixture, fixture.workspaceID, 0, 15)
	windowReserved := budgetCount(t, fixture.pool, `SELECT reserved_micro_usd FROM governance_budget_window WHERE workspace_id = $1`, fixture.workspaceID)
	windowSpent := budgetCount(t, fixture.pool, `SELECT spent_micro_usd FROM governance_budget_window WHERE workspace_id = $1`, fixture.workspaceID)
	if windowReserved != 0 || windowSpent != 15 {
		t.Fatalf("window reserved/spent = %d/%d, want 0/15", windowReserved, windowSpent)
	}
}

func TestSettleEvaluationUnknownUsageChargesReservedCap(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	policy := testDeploymentBudgetPolicy(60, 100, 100)
	input := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 3, 100, 1)
	reservation, err := fixture.service.ReserveEvaluation(context.Background(), policy, input)
	if err != nil {
		t.Fatalf("reserve evaluation: %v", err)
	}
	if err := fixture.service.SettleEvaluation(context.Background(), policy, reservation, "receipt-unknown", 0, 0, false); err != nil {
		t.Fatalf("settle unknown usage: %v", err)
	}
	assertBudgetRootTotals(t, fixture, fixture.workspaceID, 0, 60)
	var usageKnown bool
	if err := fixture.pool.QueryRow(context.Background(), `SELECT usage_known FROM governance_budget_reservation WHERE workspace_id = $1 AND reservation_id = $2`, fixture.workspaceID, reservation.Reservation.ReservationID).Scan(&usageKnown); err != nil {
		t.Fatal(err)
	}
	if usageKnown {
		t.Fatal("unknown provider usage was recorded as known")
	}
}

func TestReserveEvaluationAllowsOnlyOneConcurrentFinalCreditClaim(t *testing.T) {
	fixture := newBudgetTestFixture(t)
	policy := testDeploymentBudgetPolicy(60, 100, 100)
	inputs := []EvaluationBudgetInput{
		testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 3, 100, 1),
		testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 3, 100, 1),
	}
	start := make(chan struct{})
	results := make(chan error, len(inputs))
	var workers sync.WaitGroup
	for _, input := range inputs {
		input := input
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := fixture.service.ReserveEvaluation(context.Background(), policy, input)
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	accepted, rejected := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrBudgetLimit):
			rejected++
		default:
			t.Fatalf("concurrent admission error = %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("admissions accepted/rejected = %d/%d, want 1/1", accepted, rejected)
	}
	count := budgetCount(t, fixture.pool, `SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1`, fixture.workspaceID)
	if count != 1 {
		t.Fatalf("reservation rows = %d, want 1", count)
	}
	assertBudgetRootTotals(t, fixture, fixture.workspaceID, 60, 0)
}

func TestReserveEvaluationPreservesDefaultOffAndEpochFence(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings string
		epoch    int64
		wantErr  error
	}{
		{name: "default off", settings: `{"jev_governance_enabled":false,"rule_mode":"off"}`, epoch: 1, wantErr: ErrConcurrencyControl},
		{name: "stale epoch", settings: `{"jev_governance_enabled":true,"rule_mode":"shadow"}`, epoch: 0, wantErr: ErrConcurrencyControl},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBudgetTestFixture(t)
			if _, err := fixture.pool.Exec(context.Background(), `UPDATE governance_workspace_config SET settings = $2 WHERE workspace_id = $1`, fixture.workspaceID, test.settings); err != nil {
				t.Fatal(err)
			}
			input := testEvaluationBudgetInput(fixture.workspaceID, budgetTestUUID(), budgetTestUUID(), 2, 100, test.epoch)
			_, err := fixture.service.ReserveEvaluation(context.Background(), testDeploymentBudgetPolicy(40, 100, 100), input)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("admission error = %v, want %v", err, test.wantErr)
			}
			assertBudgetNoArtifacts(t, fixture, input.AttemptID)
		})
	}
}

func testDeploymentBudgetPolicy(attemptCap, windowCap, workspaceCap int64) *DeploymentBudgetPolicy {
	return testDeploymentBudgetPolicyWithConcurrency(attemptCap, windowCap, workspaceCap, 10)
}

func testDeploymentBudgetPolicyWithConcurrency(attemptCap, windowCap, workspaceCap, maxConcurrentEvaluations int64) *DeploymentBudgetPolicy {
	return &DeploymentBudgetPolicy{
		Version: "test-v1", Provider: "jev", Model: "test-model",
		InputMicroUSDPerMillionTokens: 100, OutputMicroUSDPerMillionTokens: 200,
		MaxAttemptCostMicroUSD: attemptCap, MaxWindowSpendMicroUSD: windowCap,
		MaxWorkspaceSpendMicroUSD: workspaceCap, MaxWindowSeconds: 3600, MaxEvaluationsPerCase: 10,
		MaxConcurrentEvaluations: maxConcurrentEvaluations,
	}
}

func testEvaluationBudgetInput(workspaceID, caseID, attemptID pgtype.UUID, maxEvaluations, workspaceSpendCap, controlEpoch int64) EvaluationBudgetInput {
	windowSeconds := int64(3600)
	return EvaluationBudgetInput{
		WorkspaceID: workspaceID, CaseID: caseID, AttemptID: attemptID, ObligationID: attemptID,
		ControlEpoch: controlEpoch,
		Limits: OperatingLimits{
			MaxEvaluations: &maxEvaluations, AdmissionWindowSeconds: &windowSeconds,
			WorkspaceSpendCapMicroUSD: &workspaceSpendCap,
			PricingPolicy:             &PricingPolicy{Version: "test-v1"},
		},
	}
}
