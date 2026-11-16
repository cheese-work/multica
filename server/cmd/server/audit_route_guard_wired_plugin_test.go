package main

// CHE-5649: workspace plugin routes whose handlers write the audit row in the
// service's own transaction. Both publish routes write plugin.package.publish.
// POST /plugins/preview only reads a manifest and changes nothing, so it is
// exempt rather than audited.
func init() {
	for route, kind := range map[string]string{
		"POST /api/workspaces/{id}/plugins":                                     "plugin.install",
		"DELETE /api/workspaces/{id}/plugins/{installationId}":                  "plugin.uninstall",
		"POST /api/workspaces/{id}/plugins/{installationId}/enable":             "plugin.enable",
		"POST /api/workspaces/{id}/plugins/{installationId}/disable":            "plugin.disable",
		"PUT /api/workspaces/{id}/plugins/{installationId}/config":              "plugin.config.update",
		"POST /api/workspaces/{id}/plugins/{installationId}/token":              "plugin.token.rotate",
		"DELETE /api/workspaces/{id}/plugins/{installationId}/token":            "plugin.token.revoke",
		"PUT /api/workspaces/{id}/plugins/{installationId}/mcp/{hookKey}/tools": "plugin.mcp_tools.approve",
		"POST /api/workspaces/{id}/plugins/packages":                            "plugin.package.publish",
		"POST /api/workspaces/{id}/plugins/packages/local":                      "plugin.package.publish",
		"DELETE /api/workspaces/{id}/plugins/packages/{packageId}":              "plugin.package.delete",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
	const preview = "POST /api/workspaces/{id}/plugins/preview"
	auditExemptRoutes[preview] = "read-only: fetches and parses a plugin manifest, no state change"
	delete(auditPendingL5b, preview)
}
