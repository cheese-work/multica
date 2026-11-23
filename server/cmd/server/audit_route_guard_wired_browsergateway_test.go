package main

// CHE-8883 (A1b): per-gateway browser credential routes. The handler writes the audit row in the
// issue or revoke transaction (browserlease.CredentialService.Audit).
func init() {
	for route, kind := range map[string]string{
		"POST /api/workspaces/{id}/browser-gateways/{gateway}/credentials":                  "browser.gateway_credential.issue",
		"DELETE /api/workspaces/{id}/browser-gateways/{gateway}/credentials/{credentialId}": "browser.gateway_credential.revoke",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
}
