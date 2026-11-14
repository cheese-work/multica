package handler

import (
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for autopilot collaborators, deliveries, manual runs and webhook
// credentials, and the first workspace-config routes (CHE-5648, E3 of CHE-4807).
// Default-deny: a field outside Allow is stored as the changed marker, so no
// webhook token, signing secret or job config reaches a row.
func init() {
	for _, kind := range []string{"autopilot.collaborator.add", "autopilot.collaborator.remove"} {
		auditKinds[kind] = auditKindSpec{TargetType: "autopilot", Allow: []string{"user_id"}}
	}
	auditKinds["autopilot.delivery.replay"] = auditKindSpec{
		TargetType: "autopilot",
		Allow:      []string{"delivery_id", "replayed_from_delivery_id", "trigger_id"},
	}
	// A manual run is recorded as the request: the run row is created by the
	// dispatcher, which owns its own transaction.
	auditKinds["autopilot.trigger.manual"] = auditKindSpec{TargetType: "autopilot", Allow: []string{"execution_mode"}}
	// webhook_token and signing_secret are not allowlisted. has_signing_secret
	// is the only fact about the secret that is stored.
	secrets := auditKindSpec{
		TargetType: "autopilot_trigger",
		Allow:      []string{"autopilot_id", "kind", "provider", "has_signing_secret"},
	}
	for _, kind := range []string{"autopilot.trigger.rotate_token", "autopilot.trigger.signing_secret"} {
		auditKinds[kind] = secrets
	}

	auditKinds["workspace.decision_gate.update"] = auditKindSpec{
		TargetType: "workspace",
		Allow:      []string{"decision_passkey_gate", "policy_revision"},
	}
	// statuses and overrides are lists and maps, so a change stores the marker.
	auditKinds["workspace.routing_agent.update"] = auditKindSpec{
		TargetType: "workspace",
		Allow:      []string{"agent_id", "fallback_agent_id"},
	}
	// Each job is a map of agent ids and limits: a change names the job and
	// stores the marker.
	for _, kind := range []string{"workspace.background_agents.update", "workspace.background_agents.reset"} {
		auditKinds[kind] = auditKindSpec{TargetType: "workspace"}
	}
	for _, kind := range []string{"workspace.scheduling.pause", "workspace.scheduling.resume"} {
		auditKinds[kind] = auditKindSpec{TargetType: "workspace", Allow: []string{"scheduling_paused", "scheduling_paused_by"}}
	}
	auditKinds["workspace.provider_breaker.reset"] = auditKindSpec{
		TargetType: "workspace",
		Allow:      []string{"provider", "state"},
	}
}

// workspaceSchedulingAuditFields snapshots the pause state of a workspace row.
func workspaceSchedulingAuditFields(w db.Workspace) map[string]any {
	return map[string]any{
		"scheduling_paused":    w.SchedulingPausedAt.Valid,
		"scheduling_paused_by": optionalUUIDString(w.SchedulingPausedBy),
	}
}

// routingAgentAuditFields snapshots the routing-agent setting.
func routingAgentAuditFields(cfg RoutingAgentConfig) map[string]any {
	return map[string]any{
		"agent_id":          cfg.AgentID,
		"fallback_agent_id": cfg.FallbackAgentID,
		"statuses":          cfg.Statuses,
		"overrides":         cfg.Overrides,
	}
}

// triggerSecretAuditFields snapshots a webhook trigger's credentials. The token
// and secret values are not allowlisted, so a change stores the marker.
func triggerSecretAuditFields(t db.AutopilotTrigger) map[string]any {
	return map[string]any{
		"autopilot_id":       uuidToString(t.AutopilotID),
		"kind":               t.Kind,
		"provider":           t.Provider,
		"webhook_token":      t.WebhookToken.String,
		"signing_secret":     t.SigningSecret.String,
		"has_signing_secret": t.SigningSecret.Valid && t.SigningSecret.String != "",
	}
}
