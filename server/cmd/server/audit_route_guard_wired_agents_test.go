package main

// CHE-5646: agent admin routes whose handlers write the audit row in the
// mutation's transaction. POST /api/agents/mika writes agent.create and audits
// only the first call (the get-or-create returns the existing Mika unchanged).
func init() {
	for route, kind := range map[string]string{
		"POST /api/agents/{id}/labels":                        "agent.label.attach",
		"DELETE /api/agents/{id}/labels/{labelId}":            "agent.label.detach",
		"DELETE /api/agents/{id}/mcp-servers/{serverId}":      "agent.mcp_server.remove",
		"PUT /api/agents/{id}/mcp-servers/{serverId}/enabled": "agent.mcp_server.enable",
		"DELETE /api/agents/{id}/skills/{skillId}":            "agent.skills.update",
		"PUT /api/agents/{id}/skills/{skillId}/enabled":       "agent.skill.enable",
		"POST /api/agents/{id}/presets":                       "agent.preset.create",
		"PATCH /api/agents/{id}/presets/{name}":               "agent.preset.update",
		"DELETE /api/agents/{id}/presets/{name}":              "agent.preset.delete",
		"PUT /api/agents/{id}/presets/default":                "agent.preset.default",
		"POST /api/agents/{id}/archive":                       "agent.archive",
		"POST /api/agents/{id}/restore":                       "agent.restore",
		"POST /api/agents/{id}/pause":                         "agent.pause",
		"POST /api/agents/{id}/resume":                        "agent.resume",
		"POST /api/agents/{id}/cancel-tasks":                  "agent.tasks.cancel",
		"POST /api/agents/mika":                               "agent.create",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
}
