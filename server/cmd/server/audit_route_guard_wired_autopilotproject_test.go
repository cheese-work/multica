package main

// CHE-2774 L5a: autopilot, trigger, project, workflow and resource routes whose
// handlers write the audit row in the mutation's transaction.
func init() {
	for route, kind := range map[string]string{
		"POST /api/autopilots/":                             "autopilot.create",
		"PATCH /api/autopilots/{id}/":                       "autopilot.update",
		"DELETE /api/autopilots/{id}/":                      "autopilot.delete",
		"POST /api/autopilots/{id}/triggers":                "autopilot.trigger.create",
		"PATCH /api/autopilots/{id}/triggers/{triggerId}/":  "autopilot.trigger.update",
		"DELETE /api/autopilots/{id}/triggers/{triggerId}/": "autopilot.trigger.delete",
		"POST /api/projects/":                               "project.create",
		"PUT /api/projects/{id}/":                           "project.update",
		"DELETE /api/projects/{id}/":                        "project.delete",
		"POST /api/projects/{id}/workflow":                  "project.workflow.update",
		"POST /api/projects/{id}/resources":                 "project.resource.create",
		"PUT /api/projects/{id}/resources/{resourceId}":     "project.resource.update",
		"DELETE /api/projects/{id}/resources/{resourceId}":  "project.resource.delete",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}
}
