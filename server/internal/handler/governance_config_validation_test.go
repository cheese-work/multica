package handler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/governance"
)

func TestValidGovernanceConfigPatchPricingPolicy(t *testing.T) {
	zero := int64(0)
	expectedVersion := int64(0)
	valid := governanceConfigPatch{
		RequestID:       uuid.NewString(),
		ExpectedVersion: &expectedVersion,
		Limits: &governanceLimitsPatch{PricingPolicy: &governance.PricingPolicy{
			Version:                       "catalog-v1",
			InputMicroUSDPerMillionTokens: &zero,
		}},
	}
	if !validGovernanceConfigPatch(valid) {
		t.Fatal("valid partial pricing policy patch was rejected")
	}
	invalid := valid
	invalid.Limits = &governanceLimitsPatch{PricingPolicy: &governance.PricingPolicy{
		Version:                        "catalog-v1",
		OutputMicroUSDPerMillionTokens: negativePointer(),
	}}
	if validGovernanceConfigPatch(invalid) {
		t.Fatal("negative pricing was accepted")
	}
}

func negativePointer() *int64 {
	value := int64(-1)
	return &value
}

func TestGovernanceProblemCatalogCoversConfigErrors(t *testing.T) {
	for _, code := range []string{
		"revision_conflict", "invalid_request", "agent_denied", "permission_denied",
		"unauthorized", "workspace_not_found", "configuration_unavailable",
		"idempotency_conflict", "rollback_unavailable", "stale_control_epoch",
	} {
		problem, ok := governance.SafeProblemFor(code)
		if !ok || problem.Problem == "" || problem.Cause == "" || problem.PermittedFix == "" || problem.DocumentationLink == "" {
			t.Errorf("safe problem %q is incomplete: %+v, found=%v", code, problem, ok)
		}
	}
}
