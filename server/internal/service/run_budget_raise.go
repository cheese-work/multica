package service

import (
	"errors"
	"math"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

var (
	// ErrRunBudgetMaxReached means no cap within the platform maximum can give
	// the issue the runs asked for.
	ErrRunBudgetMaxReached = errors.New("run budget: platform maximum reached")
	// ErrRunBudgetBelowUsed means the new cap would not clear the runs already
	// used, so the issue would stay blocked.
	ErrRunBudgetBelowUsed = errors.New("run budget: target is not above the runs already used")

	errRunBudgetAddBelowOne = errors.New("run budget: add at least one run")
)

// suggestedRaiseRuns is how many runs a suggested raise leaves.
const suggestedRaiseRuns = 10

// RunBudgetRaise is what a raise does to one issue's budget.
type RunBudgetRaise struct {
	Delta         int // runs added to the credited limit
	CreditedAfter int // credited limit after the raise
	BalanceAfter  int // runs left after the raise
	MaxAllowedAdd int // largest add the platform maximum still accepts
}

// MaxAllowedRaise is the largest number of runs a raise can leave: the room
// between the platform maximum and the larger of the limit and the runs used.
func MaxAllowedRaise(credited, used int) int {
	return max(0, issueworkflow.MaxRunsPerIssueLimit-max(credited, used))
}

// SuggestedRaise is the add a client offers: 10, or what the maximum allows.
func SuggestedRaise(credited, used int) int {
	return min(suggestedRaiseRuns, MaxAllowedRaise(credited, used))
}

// PreviewRunBudgetRaise computes "allow add more runs": the issue has add runs
// left after the raise, runs already used (including runs past the limit) are
// paid off first. The preview, the applied change and the audit row all use
// this one result, so they cannot disagree.
func PreviewRunBudgetRaise(credited, used, add int) (RunBudgetRaise, error) {
	if add < 1 {
		return RunBudgetRaise{}, errRunBudgetAddBelowOne
	}
	room := MaxAllowedRaise(credited, used)
	if add > room {
		return RunBudgetRaise{MaxAllowedAdd: room}, ErrRunBudgetMaxReached
	}
	after := max(credited, used) + add
	return RunBudgetRaise{Delta: after - credited, CreditedAfter: after, BalanceAfter: after - used, MaxAllowedAdd: room}, nil
}

// PreviewRunBudgetRaiseTo is the absolute form: set the limit to target. A
// target at or below the runs already used is refused.
func PreviewRunBudgetRaiseTo(credited, used, target int) (RunBudgetRaise, error) {
	if target > issueworkflow.MaxRunsPerIssueLimit {
		return RunBudgetRaise{MaxAllowedAdd: MaxAllowedRaise(credited, used)}, ErrRunBudgetMaxReached
	}
	if target <= max(credited, used) {
		return RunBudgetRaise{}, ErrRunBudgetBelowUsed
	}
	return PreviewRunBudgetRaise(credited, used, target-max(credited, used))
}

// raiseUsed is the runs the raise math treats as spent: the cap less the
// admitted balance. A grant or refund lowers it, carried debt raises it past
// the cap, so a raise from this base leaves the balance it promises. The
// window's task count is usage history, not an input.
func raiseUsed(credited int, balance int64) int {
	return int(max(min(int64(credited)-balance, int64(issueworkflow.MaxRunsPerIssueLimit)), math.MinInt32))
}

// SuggestedRaiseForBalance is the absolute cap the suggested raise sets and the
// balance it leaves, from the cap and the admitted balance. It is 0, 0 when the
// platform maximum leaves no valid raise.
func SuggestedRaiseForBalance(credited int, balance int64) (target, after int) {
	used := raiseUsed(credited, balance)
	if MaxAllowedRaise(credited, used) == 0 {
		return 0, 0
	}
	raise, err := PreviewRunBudgetRaise(credited, used, SuggestedRaise(credited, used))
	if err != nil {
		return 0, 0
	}
	return raise.CreditedAfter, raise.BalanceAfter
}
