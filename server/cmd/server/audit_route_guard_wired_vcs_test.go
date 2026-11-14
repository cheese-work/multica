package main

// CHE-4807 E7: VCS integration routes whose handlers write the audit row in the
// mutation's transaction.
func init() {
	for route, kind := range map[string]string{
		"DELETE /api/workspaces/{id}/github/installations/{installationId}":       "github.installation.delete",
		"POST /api/workspaces/{id}/vcs/connections":                               "vcs.connection.create",
		"DELETE /api/workspaces/{id}/vcs/connections/{connectionId}":              "vcs.connection.delete",
		"POST /api/workspaces/{id}/vcs/connections/{connectionId}/rotate-webhook": "vcs.webhook.rotate",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
}
