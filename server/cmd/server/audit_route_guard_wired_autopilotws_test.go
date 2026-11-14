package main

// CHE-5648 (E3 of CHE-4807): autopilot collaborator, delivery, manual-run and
// webhook-credential routes, and the first workspace-config routes, whose
// handlers write the audit row in the mutation's transaction.
func init() {
	for route, kind := range map[string]string{
		"POST /api/autopilots/{id}/collaborators":                             "autopilot.collaborator.add",
		"DELETE /api/autopilots/{id}/collaborators/{userId}":                  "autopilot.collaborator.remove",
		"POST /api/autopilots/{id}/deliveries/{deliveryId}/replay":            "autopilot.delivery.replay",
		"POST /api/autopilots/{id}/trigger":                                   "autopilot.trigger.manual",
		"POST /api/autopilots/{id}/triggers/{triggerId}/rotate-webhook-token": "autopilot.trigger.rotate_token",
		"PUT /api/autopilots/{id}/triggers/{triggerId}/signing-secret":        "autopilot.trigger.signing_secret",
		"PUT /api/workspaces/{id}/decision-gate":                              "workspace.decision_gate.update",
		"PUT /api/workspaces/{id}/routing-agent":                              "workspace.routing_agent.update",
		"PUT /api/workspaces/{id}/background-agents":                          "workspace.background_agents.update",
		"DELETE /api/workspaces/{id}/background-agents":                       "workspace.background_agents.reset",
		"POST /api/workspaces/{id}/pause":                                     "workspace.scheduling.pause",
		"POST /api/workspaces/{id}/resume":                                    "workspace.scheduling.resume",
		"POST /api/workspaces/{id}/provider-breakers/{provider}/reset":        "workspace.provider_breaker.reset",
	} {
		auditedRoutes[route] = kind
		delete(auditPendingL5b, route)
	}

	// The challenge only issues a one-time assertion challenge. It changes no
	// workspace state; the gate change it leads to is audited above.
	const challenge = "POST /api/workspaces/{id}/decision-gate/challenge"
	auditExemptRoutes[challenge] = "issues a one-time passkey challenge and changes no workspace state; the signed gate change is audited as workspace.decision_gate.update"
	delete(auditPendingL5b, challenge)
}
