package main

func init() {
	for route, kind := range map[string]string{
		"POST /api/workspaces/{id}/runtime-profiles":               "runtime_profile.create",
		"PATCH /api/workspaces/{id}/runtime-profiles/{profileId}":  "runtime_profile.update",
		"PUT /api/workspaces/{id}/runtime-profiles/{profileId}":    "runtime_profile.update",
		"DELETE /api/workspaces/{id}/runtime-profiles/{profileId}": "runtime_profile.delete",
		"POST /api/workspaces/{id}/mcp-servers":                    "mcp_server.create",
		"PUT /api/workspaces/{id}/mcp-servers/{serverId}":          "mcp_server.update",
		"DELETE /api/workspaces/{id}/mcp-servers/{serverId}":       "mcp_server.delete",
		"POST /api/agents/":                    "agent.create",
		"PATCH /api/runtimes/{runtimeId}/":     "runtime.rename",
		"POST /api/workspaces/{id}/qoder/stop": "qoder.stop",
		"DELETE /api/workspaces/{id}/qoder":    "qoder.disconnect",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
}
