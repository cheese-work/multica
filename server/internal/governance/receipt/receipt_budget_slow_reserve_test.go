package receipt

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/governance"
)

// slowReserveAdmission delays ReserveEvaluation past the production Budget
// before delegating to the real BudgetService. It makes the "cold reserve
// outlasts 50ms" condition deterministic instead of depending on host load.
type slowReserveAdmission struct {
	BudgetAdmission
	delay time.Duration
}

func (s slowReserveAdmission) ReserveEvaluation(ctx context.Context, p *governance.DeploymentBudgetPolicy, in governance.EvaluationBudgetInput) (governance.BudgetReservationResult, error) {
	time.Sleep(s.delay)
	return s.BudgetAdmission.ReserveEvaluation(ctx, p, in)
}

func slowReserveObserver(fixture *receiptBudgetFixture) *Observer {
	return &Observer{
		Provider:        &fakeProvider{resp: mentionOwnerResponse(0.99)},
		Store:           &fakeStore{},
		BudgetAdmission: slowReserveAdmission{BudgetAdmission: fixture.service, delay: 2 * Budget},
		BudgetPolicy:    receiptTestBudgetPolicy(),
	}
}

func slowReserveInput(fixture *receiptBudgetFixture) Input {
	return Input{
		WorkspaceID:     fixture.workspaceID,
		ControlEpoch:    1,
		Limits:          receiptTestOperatingLimits(),
		IssueID:         receiptTestUUID(),
		CommentID:       receiptTestUUID(),
		CommentRevision: 1,
		Trigger:         TriggerCreate,
		Eval:            oneCandidateOneSpanInput(),
	}
}

// CHE-1578: the real-budget tests assert reservation identity, not latency, so
// their helper must not be shed by a reserve that outlasts the production
// Budget. A reserve of 2*Budget stands in for a cold pool connection under
// -race or host load.
func TestObserveRealBudgetHelperToleratesReserveSlowerThanBudget(t *testing.T) {
	fixture := newReceiptBudgetFixture(t)
	observer := slowReserveObserver(fixture)

	result := observeRealBudget(t, observer, slowReserveInput(fixture))
	observer.WaitForIdle()

	if result.Status != "decided" {
		t.Fatalf("observation with a %v reserve = %+v, want status decided (the real-budget helper must lift the production Budget)", 2*Budget, result)
	}
}

// CHE-946 guard: the production path, with no override, still sheds when the
// reserve outlasts Budget. This keeps the fix from loosening the clamp.
func TestObserveProductionBudgetStillShedsSlowReserve(t *testing.T) {
	fixture := newReceiptBudgetFixture(t)
	observer := slowReserveObserver(fixture)

	result := observer.Observe(context.Background(), slowReserveInput(fixture))
	observer.WaitForIdle()

	if result.Status != "shed" || result.ShedReason != ReasonBudgetExceeded {
		t.Fatalf("production-budget observation with a %v reserve = %+v, want shed/budget_exceeded", 2*Budget, result)
	}
}
