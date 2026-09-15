package checkpoint

// EvaluateAgainstScan runs the full invalidation rule the content contract
// requires: compare live revisions/candidate identity first, then diff
// per-thread coverage. Returns the thread ids that must be expanded this turn
// and whether the checkpoint should be treated as stale overall.
//
// When CandidateChanged is true, every live thread is returned as changed —
// a new candidate or materially updated issue means the checkpoint's own
// accepted-decisions/obligations fields may be stale, not just thread
// coverage, so partial reuse is not safe.
func EvaluateAgainstScan(cp Checkpoint, liveIssueRev int64, liveCandidateID string, live []ThreadCoverage) (changedThreadIDs []string, stale bool) {
	if CandidateChanged(cp, liveIssueRev, liveCandidateID) {
		ids := make([]string, 0, len(live))
		for _, lt := range live {
			ids = append(ids, lt.ThreadRootID)
		}
		return ids, true
	}
	return Diff(cp, live)
}
