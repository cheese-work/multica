package main

// CHE-5647 (CHE-4807 E2): runtime, skill and project routes whose handlers write
// the audit row in the mutation's transaction. Skill file writes record
// skill.update. Only the project scope of the wakeup-definition API is wired;
// the workspace and issue scopes stay pending.
func init() {
	for route, kind := range map[string]string{
		"PUT /api/runtimes/{runtimeId}/pause":                    "runtime.pause",
		"DELETE /api/runtimes/{runtimeId}/pause":                 "runtime.resume",
		"POST /api/runtimes/{runtimeId}/update":                  "runtime.update",
		"POST /api/runtimes/{runtimeId}/local-skills/import":     "runtime.local_skill.import",
		"PUT /api/skills/{id}/files":                             "skill.update",
		"DELETE /api/skills/{id}/files/{fileId}":                 "skill.update",
		"POST /api/skills/{id}/labels":                           "skill.label.attach",
		"DELETE /api/skills/{id}/labels/{labelId}":               "skill.label.detach",
		"POST /api/projects/{id}/wakeup-definitions/":            "project.wakeup_definition.create",
		"PUT /api/projects/{id}/wakeup-definitions/{rule}":       "project.wakeup_definition.update",
		"DELETE /api/projects/{id}/wakeup-definitions/{rule}":    "project.wakeup_definition.delete",
		"PUT /api/projects/{id}/github-route-grants/{userId}":    "project.route_grant.set",
		"DELETE /api/projects/{id}/github-route-grants/{userId}": "project.route_grant.remove",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
	for route, reason := range map[string]string{
		"POST /api/runtimes/{runtimeId}/models":              "asks the daemon to list models; stores a transient request and changes no configuration",
		"POST /api/runtimes/{runtimeId}/local-skills":        "asks the daemon to list local skills; stores a transient request and changes no configuration",
		"POST /api/projects/{id}/wakeup-definitions/preview": "resolves a proposed rule without saving it; writes nothing",
	} {
		auditExemptRoutes[route] = reason
		delete(auditPendingL5b, route)
	}
}
