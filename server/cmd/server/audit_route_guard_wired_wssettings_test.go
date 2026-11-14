package main

// CHE-5655 (E4 of CHE-4807): the last workspace-settings routes. The wired
// routes write the audit row in the mutation's transaction; the exempt routes
// change no audited state, or record it in their own audit table.
func init() {
	for route, kind := range map[string]string{
		"PUT /api/workspaces/{id}/protected-path-policies/{name}": "workspace.protected_path_policy.update",
		"DELETE /api/workspaces/{id}/share-links/{linkId}":        "share_link.revoke",
		"POST /api/workspaces/":                                   "workspace.create",
		"PUT /api/workspaces/{id}/qoder":                          "qoder.save",
		"POST /api/workspaces/{id}/qoder/resume":                  "qoder.resume",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}

	for route, reason := range map[string]string{
		"POST /api/workspaces/{id}/qoder/check":        "calls the Qoder API with a PAT to test it and stores nothing; the PAT is saved, and audited, by qoder.save",
		"POST /api/workspaces/{id}/qoder/environments": "lists Qoder environments with a PAT and stores nothing; the chosen environment is saved, and audited, by qoder.save",
		"PUT /api/github/dispatch-halt":                "records the halt and its actor in github_dispatch_halt_audit in the same transaction",
		"POST /api/github/intake/revisions/{id}/approve": "records the approver and snapshot hash in github_snapshot_approval in the same transaction",
	} {
		auditExemptRoutes[route] = reason
		delete(auditPendingL5b, route)
	}
}
