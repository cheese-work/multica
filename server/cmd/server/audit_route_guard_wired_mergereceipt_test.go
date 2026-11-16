package main

// CHE-6454: PR merge-eligibility facts (CHE-5594). They are per-PR rows keyed by
// repository and PR number, not workspace settings, and no agent reach changes
// here: a receipt or veto only gates a merge. Each route has its own
// accountable record, so none writes an audit row.
func init() {
	for route, reason := range map[string]string{
		"PUT /api/github/merge-receipts":    "per-PR receipt facts (head SHA, tier, gates, evidence) stored with the server's observation time; a person who is owner or admin only, and the row is the record. Changes no workspace setting or agent reach (borderline)",
		"PUT /api/github/merge-receipts/ci": "per-PR receipt written by the pinned CI workflow, authenticated by its GitHub Actions OIDC token; the broker pins the workflow and repository and the row is the record. Not an admin action",
		"PUT /api/github/merge-vetoes":      "per-PR veto row that keeps who set it, when and why; holds one PR and changes no workspace setting or agent reach (borderline)",
		"DELETE /api/github/merge-vetoes":   "lifts one PR's veto; the PR stays held until a producer records a new receipt. Changes no workspace setting or agent reach (borderline, the lift itself keeps no row)",
	} {
		auditExemptRoutes[route] = reason
	}
}
