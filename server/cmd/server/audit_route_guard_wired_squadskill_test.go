package main

// CHE-2774 L5a: squad, skill and agent skill/MCP binding routes whose handlers
// write the audit row in the mutation's transaction. POST /api/skills/import
// writes skill.create (create, rename) or skill.update (on_conflict overwrite).
func init() {
	for route, kind := range map[string]string{
		"POST /api/squads/":                           "squad.create",
		"PUT /api/squads/{id}/":                       "squad.update",
		"DELETE /api/squads/{id}/":                    "squad.delete",
		"POST /api/squads/{id}/members":               "squad.member.add",
		"DELETE /api/squads/{id}/members":             "squad.member.remove",
		"PATCH /api/squads/{id}/members/role":         "squad.member.role.update",
		"POST /api/skills/":                           "skill.create",
		"POST /api/skills/import":                     "skill.create",
		"PUT /api/skills/{id}/":                       "skill.update",
		"DELETE /api/skills/{id}/":                    "skill.delete",
		"POST /api/skills/{id}/refresh":               "skill.update",
		"PUT /api/agents/{id}/skills":                 "agent.skills.update",
		"POST /api/agents/{id}/skills/add":            "agent.skills.update",
		"PUT /api/agents/{id}/runtime-skills/enabled": "agent.runtime_skill.update",
		"POST /api/agents/{id}/mcp-servers":           "agent.mcp_server.add",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
}
