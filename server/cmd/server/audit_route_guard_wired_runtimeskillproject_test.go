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
		"PUT /api/runtimes/{runtimeId}/update-schedule":          "runtime.update_schedule.set",
		"DELETE /api/runtimes/{runtimeId}/update-schedule":       "runtime.update_schedule.clear",
		"POST /api/runtimes/{runtimeId}/local-skills/import":     "runtime.local_skill.import",
		"PUT /api/skills/{id}/files":                             "skill.update",
		"DELETE /api/skills/{id}/files/{fileId}":                 "skill.update",
		"POST /api/skills/{id}/versions/{version}/restore":       "skill.update",
		"POST /api/skills/{id}/labels":                           "skill.label.attach",
		"DELETE /api/skills/{id}/labels/{labelId}":               "skill.label.detach",
		"POST /api/projects/{id}/wakeup-definitions/":            "project.wakeup_definition.create",
		"PUT /api/projects/{id}/wakeup-definitions/{rule}":       "project.wakeup_definition.update",
		"DELETE /api/projects/{id}/wakeup-definitions/{rule}":    "project.wakeup_definition.delete",
		"PUT /api/projects/{id}/github-route-grants/{userId}":    "project.route_grant.set",
		"DELETE /api/projects/{id}/github-route-grants/{userId}": "project.route_grant.remove",
		"POST /api/projects/{id}/archive":                        "project.archive",
		"POST /api/projects/{id}/restore":                        "project.restore",
		"POST /api/projects/{id}/members":                        "project.member.add",
		"POST /api/projects/{id}/join":                           "project.member.add",
		"DELETE /api/projects/{id}/members/{userId}":             "project.member.remove",
		"PUT /api/agents/{id}/apps":                              "agent.apps.update",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
	for route, reason := range map[string]string{
		"POST /api/runtimes/{runtimeId}/models":              "asks the daemon to list models; stores a transient request and changes no configuration",
		"POST /api/runtimes/{runtimeId}/local-skills":        "asks the daemon to list local skills; stores a transient request and changes no configuration",
		"POST /api/projects/{id}/wakeup-definitions/preview": "resolves a proposed rule without saving it; writes nothing",
		"POST /api/projects/workflow":                        "CHE-10137: opens one owner decision card for many project switches and writes no project; approval writes one project.workflow.update row per project in its transaction",
		// CHE-7023: routes the v0.10.0 merge and the fork's merge gate left unclassified.
		"PUT /api/projects/{id}/subscription":              "the caller's own notification subscription; no workspace setting, project access or agent reach changes",
		"DELETE /api/projects/{id}/subscription":           "the caller's own notification subscription; no workspace setting, project access or agent reach changes",
		"PUT /api/workspaces/{id}/pin":                     "the caller's own sidebar pin of a workspace; stored per member, not a workspace setting",
		"POST /api/skills/{id}/versions/{version}/restore": "restoring a skill version writes a new skill_version row (source restore, restored_from, created_by) in the same transaction; the version history is the trail",
		"PUT /api/workspaces/{id}/ci-watch/repos/{repo}":   "ApplyConfig writes one ci_watch_config_audit row per changed field (actor, old and new value) in the same transaction",
		"PUT /api/github/merge-receipts":                   "merge-gate fact for one PR head, stored with its evidence and observation time; owner or admin person only (CHE-5594); not a workspace setting or agent grant",
		"PUT /api/github/merge-receipts/ci":                "merge-gate fact written by a pinned GitHub Actions OIDC identity for one repository; not a workspace setting or agent grant",
		"PUT /api/github/merge-vetoes":                     "per-PR merge hold; merge_veto stores vetoed_by, reason and time; owner or admin person only (CHE-5594)",
		"DELETE /api/github/merge-vetoes":                  "lifts a per-PR merge hold; the PR stays held until a producer records a new receipt (CHE-5594); owner or admin person only",
	} {
		auditExemptRoutes[route] = reason
		delete(auditPendingL5b, route)
	}
}
