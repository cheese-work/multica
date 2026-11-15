package main

// CHE-4734 (W3-5): the browser lease revoke-all route. The browser_lease audit kinds and the
// revoke audit rows belong to W3-15 (CHE-4744); until then the route is exempt here and that
// issue removes the entry. The daemon release route is not an admin route, so the guard does
// not list it.
func init() {
	for route, reason := range map[string]string{
		"POST /api/workspaces/{id}/browser-leases/revoke-all": "ends every lease of the workspace; each lease row keeps end_reason; audit kinds arrive in W3-15 (CHE-4744)",
	} {
		auditExemptRoutes[route] = reason
		delete(auditPendingL5b, route)
	}
}
