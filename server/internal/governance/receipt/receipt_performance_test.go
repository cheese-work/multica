package receipt

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/jev"
)

// deadlineProvider records the deadline of the context Evaluate receives.
type deadlineProvider struct {
	calls    atomic.Int32
	deadline atomic.Value // time.Time
}

func (p *deadlineProvider) Evaluate(ctx context.Context, _ jev.Request) (*jev.Response, error) {
	p.calls.Add(1)
	if d, ok := ctx.Deadline(); ok {
		p.deadline.Store(d)
	}
	return mentionOwnerResponse(0.99), nil
}

// The 50ms work budget is one shared deadline derived before DB acquisition;
// Observe must adopt it, not restart a fresh Budget.
func TestGovernancePerformanceObserveAdoptsSharedDeadline(t *testing.T) {
	provider := &deadlineProvider{}
	o := testObserver(provider, &fakeStore{})
	in := testInput()
	in.Deadline = time.Now().Add(Budget / 2)

	o.Observe(context.Background(), in)

	got, ok := provider.deadline.Load().(time.Time)
	if !ok || !got.Equal(in.Deadline) {
		t.Fatalf("provider context deadline = %v (set=%v), want the shared %v", got, ok, in.Deadline)
	}
}

// A deadline already spent before Observe (e.g. control-lock wait) must shed
// without calling the provider or restarting the budget.
func TestGovernancePerformanceObserveShedsWhenSharedDeadlineSpent(t *testing.T) {
	provider := &deadlineProvider{}
	store := &fakeStore{}
	o := testObserver(provider, store)
	in := testInput()
	in.Deadline = time.Now().Add(-time.Millisecond)

	result := o.Observe(context.Background(), in)

	if result.Status != "shed" || result.ShedReason != ReasonBudgetExceeded {
		t.Fatalf("result = %+v, want shed/budget_exceeded", result)
	}
	if n := provider.calls.Load(); n != 0 {
		t.Fatalf("provider calls = %d, want 0", n)
	}
	o.WaitForIdle()
	if rows := store.inserted(); len(rows) != 1 || rows[0].ShedReason.String != "budget_exceeded" {
		t.Fatalf("rows = %+v, want exactly one budget_exceeded shed row (no escaped work)", rows)
	}
	if got := o.Stats().ShedBudgetExceededTotal; got != 1 {
		t.Fatalf("ShedBudgetExceededTotal = %d, want 1", got)
	}
}
