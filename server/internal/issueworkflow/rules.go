package issueworkflow

// DefaultMaxRunsPerIssue is the run budget of a workflow that sets none.
const DefaultMaxRunsPerIssue = 15

// MaxRunsPerIssueLimit is the largest cap a workflow can set.
const MaxRunsPerIssueLimit = 1000

// EffectiveMaxRuns is how many runs an issue of the workflow may use before
// the platform blocks it.
func (d Definition) EffectiveMaxRuns() int {
	if d.MaxRunsPerIssue > 0 {
		return d.MaxRunsPerIssue
	}
	return DefaultMaxRunsPerIssue
}

// BlockedReasonRunBudget is the blocked_reason metadata the engine sets when
// it blocks an issue that spent its run budget. A trigger on issue.status
// clears it when the issue leaves blocked.
const BlockedReasonRunBudget = "run-budget"
