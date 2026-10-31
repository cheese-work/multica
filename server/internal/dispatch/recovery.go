package dispatch

import "context"

type budgetRecoveryCallerKey struct{}

// WithBudgetRecoveryCaller marks ctx as a request made with the task token of a
// budget recovery run (CHE-2297 L3). Auth sets it from the stored task row; the
// run's own claims never do. Everything written under such a ctx is
// recovery-caused: no grant, no run, no child, wakeup or top-up.
func WithBudgetRecoveryCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, budgetRecoveryCallerKey{}, true)
}

// BudgetRecoveryCaller reports whether ctx belongs to a recovery run's request.
func BudgetRecoveryCaller(ctx context.Context) bool {
	v, _ := ctx.Value(budgetRecoveryCallerKey{}).(bool)
	return v
}
