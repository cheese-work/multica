package main

// CHE-4734 (W3-5): the browser lease revoke-all route. CHE-4744 (W3-15) writes the
// browser.lease.revoke rows from the Revoker transaction (browserlease.Revoker.Audit), not from
// the handler body, so the static route guard cannot see them; the route stays exempt here. The daemon release route is not an admin route, so the guard does
// not list it.
func init() {
	for route, reason := range map[string]string{
		"POST /api/workspaces/{id}/browser-leases/revoke-all": "ends every lease of the workspace; each lease ends in the Revoker, which writes one browser.lease.revoke row per lease in the same transaction (CHE-4744)",
	} {
		auditExemptRoutes[route] = reason
		delete(auditPendingL5b, route)
	}
}

// CHE-4737 (W3-8): the browser permission routes. Like revoke-all above, their audit kinds belong
// to W3-15 (CHE-4744), which removes these entries. Each grant row keeps granted_by and
// created_at until then.
func init() {
	for _, route := range []string{
		"PUT /api/workspaces/{id}/browser-profiles/{profileId}/members/{userId}",
		"DELETE /api/workspaces/{id}/browser-profiles/{profileId}/members/{userId}",
		"PUT /api/workspaces/{id}/browser-profiles/{profileId}/agents/{agentId}",
		"DELETE /api/workspaces/{id}/browser-profiles/{profileId}/agents/{agentId}",
	} {
		auditExemptRoutes[route] = "grants or removes one browser permission row (granted_by, created_at); audit kinds arrive in W3-15 (CHE-4744)"
		delete(auditPendingL5b, route)
	}
}
