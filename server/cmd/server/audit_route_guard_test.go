package main

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/realtime"
)

// Route guard for the admin audit trail (CHE-2774). Every mutating route under
// an admin prefix must be audited, on the explicit L5b pending list, or
// exempt with a reason. Adding an admin route without one of these fails the
// test; so does leaving a stale entry behind once a route is renamed or
// removed. Shrink auditPendingL5b as L5b wires routes; never grow it to make
// the test pass.

// auditAdminPrefixes are the first path segments after /api/ whose mutating
// routes configure the workspace, its agents, runtimes or integrations.
// Everything else is data-plane (issues, comments, chat, inbox, daemon, me, ...).
var auditAdminPrefixes = map[string]bool{
	"agents": true, "workspaces": true, "runtimes": true, "skills": true, "squads": true,
	"autopilots": true, "projects": true, "invitations": true, "cloud-runtime": true,
	"cloud-subscriptions": true, "cloud-billing": true, "issue-statuses": true,
	"issue-workflows": true, "labels": true, "wakeup-definitions": true, "properties": true,
	"governance": true, "integrations": true, "quick-actions": true, "tokens": true,
	"webhooks": true, "plugin-bridge": true, "agent-builder": true, "system-wakeups": true,
	"notification-channels": true, "github": true, "slack": true, "lark": true,
	"telegram": true, "wecom": true, "dingtalk": true,
}

// auditedRoutes maps "METHOD /pattern" to the audit kind its handler writes
// in the mutation's transaction.
var auditedRoutes = map[string]string{
	"PUT /api/workspaces/{id}/":                                "workspace.update",
	"PATCH /api/workspaces/{id}/":                              "workspace.update",
	"PUT /api/agents/{id}/":                                    "agent.update",
	"PUT /api/agents/{id}/env":                                 "agent.env.update",
	"PATCH /api/agents/{id}/env":                               "agent.env.update",
	"DELETE /api/runtimes/{runtimeId}/":                        "runtime.delete",
	"POST /api/runtimes/{runtimeId}/unbind-agents-and-delete":  "runtime.delete",
	"POST /api/runtimes/{runtimeId}/archive-agents-and-delete": "runtime.delete",
}

// auditExemptRoutes need no audit row, with the reason.
var auditExemptRoutes = map[string]string{
	"DELETE /api/workspaces/{id}/": "workspace deletion removes the workspace's audit rows in the same tx (audit_workspace_delete)",
}

// auditPendingL5b lists admin routes whose audit kind lands in L5b.
var auditPendingL5b = map[string]bool{}

func init() {
	for _, route := range []string{
		"DELETE /api/agents/{id}/labels/{labelId}",
		"DELETE /api/agents/{id}/mcp-servers/{serverId}",
		"DELETE /api/agents/{id}/skills/{skillId}",
		"DELETE /api/autopilots/{id}/",
		"DELETE /api/autopilots/{id}/collaborators/{userId}",
		"DELETE /api/autopilots/{id}/triggers/{triggerId}/",
		"DELETE /api/cloud-runtime/nodes",
		"DELETE /api/integrations/composio/connections/{id}",
		"DELETE /api/issue-statuses/{id}/",
		"DELETE /api/issue-workflows/{id}/",
		"DELETE /api/labels/{id}/",
		"DELETE /api/plugin-bridge/v1/storage/{scope}/{key}",
		"DELETE /api/projects/{id}/",
		"DELETE /api/projects/{id}/github-route-grants/{userId}",
		"DELETE /api/projects/{id}/resources/{resourceId}",
		"DELETE /api/projects/{id}/wakeup-definitions/{rule}",
		"DELETE /api/quick-actions/{id}/",
		"DELETE /api/runtimes/{runtimeId}/pause",
		"DELETE /api/skills/{id}/",
		"DELETE /api/skills/{id}/files/{fileId}",
		"DELETE /api/skills/{id}/labels/{labelId}",
		"DELETE /api/squads/{id}/",
		"DELETE /api/squads/{id}/members",
		"DELETE /api/tokens/{id}",
		"DELETE /api/wakeup-definitions/{rule}",
		"DELETE /api/workspaces/{id}/dingtalk/installations/{installationId}",
		"DELETE /api/workspaces/{id}/dingtalk/installations/{installationId}/groups/{conversationId}",
		"DELETE /api/workspaces/{id}/github/installations/{installationId}",
		"DELETE /api/workspaces/{id}/invitations/{invitationId}",
		"DELETE /api/workspaces/{id}/jev/credential",
		"DELETE /api/workspaces/{id}/lark/installations/{installationId}",
		"DELETE /api/workspaces/{id}/mcp-servers/{serverId}",
		"DELETE /api/workspaces/{id}/members/{memberId}/",
		"DELETE /api/workspaces/{id}/plugins/packages/{packageId}",
		"DELETE /api/workspaces/{id}/plugins/{installationId}",
		"DELETE /api/workspaces/{id}/plugins/{installationId}/token",
		"DELETE /api/workspaces/{id}/qoder",
		"DELETE /api/workspaces/{id}/runtime-profiles/{profileId}",
		"DELETE /api/workspaces/{id}/share-links/{linkId}",
		"DELETE /api/workspaces/{id}/slack/installations/{installationId}",
		"DELETE /api/workspaces/{id}/telegram/installations/{installationId}",
		"DELETE /api/workspaces/{id}/vcs/connections/{connectionId}",
		"DELETE /api/workspaces/{id}/wecom/installations/{installationId}",
		"PATCH /api/agent-builder/sessions/{sessionId}/runtime",
		"PATCH /api/autopilots/{id}/",
		"PATCH /api/autopilots/{id}/triggers/{triggerId}/",
		"PATCH /api/issue-statuses/reorder",
		"PATCH /api/issue-statuses/{id}/",
		"PATCH /api/issue-workflows/{id}/",
		"PATCH /api/notification-channels/",
		"PATCH /api/plugin-bridge/v1/issues/{issue_ref}",
		"PATCH /api/properties/{id}/",
		"PATCH /api/quick-actions/{id}/",
		"PATCH /api/runtimes/{runtimeId}/",
		"PATCH /api/squads/{id}/members/role",
		"PATCH /api/workspaces/{id}/export-privacy",
		"PATCH /api/workspaces/{id}/governance/config",
		"PATCH /api/workspaces/{id}/members/{memberId}/",
		"PATCH /api/workspaces/{id}/runtime-profiles/{profileId}",
		"POST /api/agent-builder/sessions/",
		"POST /api/agents/",
		"POST /api/agents/mika",
		"POST /api/agents/{id}/archive",
		"POST /api/agents/{id}/cancel-tasks",
		"POST /api/agents/{id}/labels",
		"POST /api/agents/{id}/mcp-servers",
		"POST /api/agents/{id}/restore",
		"POST /api/agents/{id}/skills/add",
		"POST /api/autopilots/",
		"POST /api/autopilots/{id}/collaborators",
		"POST /api/autopilots/{id}/deliveries/{deliveryId}/replay",
		"POST /api/autopilots/{id}/trigger",
		"POST /api/autopilots/{id}/triggers",
		"POST /api/autopilots/{id}/triggers/{triggerId}/rotate-webhook-token",
		"POST /api/cloud-billing/checkout-sessions",
		"POST /api/cloud-billing/portal-sessions",
		"POST /api/cloud-runtime/nodes",
		"POST /api/cloud-runtime/nodes/exec",
		"POST /api/cloud-runtime/nodes/reboot",
		"POST /api/cloud-runtime/nodes/start",
		"POST /api/cloud-runtime/nodes/status",
		"POST /api/cloud-runtime/nodes/stop",
		"POST /api/cloud-subscriptions/checkout-sessions",
		"POST /api/cloud-subscriptions/portal-sessions",
		"POST /api/cloud-subscriptions/seats/purchase-preview",
		"POST /api/cloud-subscriptions/seats/purchases",
		"POST /api/cloud-subscriptions/seats/reconcile",
		"POST /api/dingtalk/binding/redeem",
		"POST /api/governance/cases/{caseId}/export",
		"POST /api/governance/proposals/{caseId}/attempts/{attemptId}/heartbeat",
		"POST /api/governance/proposals/{caseId}/attempts/{attemptId}/result",
		"POST /api/integrations/composio/connect/init",
		"POST /api/invitations/{id}/accept",
		"POST /api/invitations/{id}/decline",
		"POST /api/issue-statuses/",
		"POST /api/issue-workflows/",
		"POST /api/issue-workflows/{id}/brief",
		"POST /api/issue-workflows/{id}/webhooks",
		"POST /api/issue-workflows/{id}/webhooks/{endpointId}/disable",
		"POST /api/issue-workflows/{id}/webhooks/{endpointId}/rotate",
		"POST /api/labels/",
		"POST /api/lark/binding/redeem",
		"POST /api/plugin-bridge/v1/hooks/{key}",
		"POST /api/plugin-bridge/v1/issues/{issue_ref}/comments",
		"POST /api/projects/",
		"POST /api/projects/{id}/resources",
		"POST /api/projects/{id}/wakeup-definitions/",
		"POST /api/projects/{id}/wakeup-definitions/preview",
		"POST /api/projects/{id}/workflow",
		"POST /api/properties/",
		"POST /api/quick-actions/",
		"POST /api/runtimes/{runtimeId}/local-skills",
		"POST /api/runtimes/{runtimeId}/local-skills/import",
		"POST /api/runtimes/{runtimeId}/models",
		"POST /api/runtimes/{runtimeId}/revoke-workspace-access",
		"POST /api/runtimes/{runtimeId}/update",
		"POST /api/skills/",
		"POST /api/skills/import",
		"POST /api/skills/{id}/labels",
		"POST /api/skills/{id}/refresh",
		"POST /api/slack/binding/redeem",
		"POST /api/squads/",
		"POST /api/squads/{id}/members",
		"POST /api/telegram/binding/redeem",
		"POST /api/tokens/",
		"POST /api/tokens/current/renew",
		"POST /api/wakeup-definitions/",
		"POST /api/wakeup-definitions/preview",
		"POST /api/webhooks/autopilots/{token}",
		"POST /api/webhooks/github",
		"POST /api/webhooks/stripe",
		"POST /api/webhooks/vcs/{connectionId}",
		"POST /api/webhooks/workflows/{endpoint_id}",
		"POST /api/wecom/binding/redeem",
		"POST /api/workspaces/",
		"POST /api/workspaces/{id}/dingtalk/install/byo",
		"POST /api/workspaces/{id}/lark/install/begin",
		"POST /api/workspaces/{id}/leave",
		"POST /api/workspaces/{id}/mcp-servers",
		"POST /api/workspaces/{id}/members",
		"POST /api/workspaces/{id}/plugins",
		"POST /api/workspaces/{id}/plugins/packages",
		"POST /api/workspaces/{id}/plugins/packages/local",
		"POST /api/workspaces/{id}/plugins/preview",
		"POST /api/workspaces/{id}/plugins/{installationId}/disable",
		"POST /api/workspaces/{id}/plugins/{installationId}/enable",
		"POST /api/workspaces/{id}/plugins/{installationId}/token",
		"POST /api/workspaces/{id}/qoder/check",
		"POST /api/workspaces/{id}/qoder/environments",
		"POST /api/workspaces/{id}/qoder/resume",
		"POST /api/workspaces/{id}/qoder/stop",
		"POST /api/workspaces/{id}/runtime-profiles",
		"POST /api/workspaces/{id}/share-links",
		"POST /api/workspaces/{id}/slack/install/byo",
		"POST /api/workspaces/{id}/telegram/install",
		"POST /api/workspaces/{id}/vcs/connections",
		"POST /api/workspaces/{id}/vcs/connections/{connectionId}/rotate-webhook",
		"POST /api/workspaces/{id}/wecom/install/byo",
		"PUT /api/agent-builder/sessions/{sessionId}/draft",
		"PUT /api/agents/{id}/mcp-servers/{serverId}/enabled",
		"PUT /api/agents/{id}/runtime-skills/enabled",
		"PUT /api/agents/{id}/skills",
		"PUT /api/agents/{id}/skills/{skillId}/enabled",
		"PUT /api/autopilots/{id}/triggers/{triggerId}/signing-secret",
		"PUT /api/github/dispatch-halt",
		"PUT /api/issue-workflows/{id}/handoffs/{status}",
		"PUT /api/labels/{id}/",
		"PUT /api/plugin-bridge/v1/storage/{scope}/{key}",
		"PUT /api/projects/{id}/",
		"PUT /api/projects/{id}/github-route-grants/{userId}",
		"PUT /api/projects/{id}/resources/{resourceId}",
		"PUT /api/projects/{id}/wakeup-definitions/{rule}",
		"PUT /api/runtimes/{runtimeId}/pause",
		"PUT /api/skills/{id}/",
		"PUT /api/skills/{id}/files",
		"PUT /api/squads/{id}/",
		"PUT /api/system-wakeups/{rule}",
		"PUT /api/wakeup-definitions/{rule}",
		"PUT /api/workspaces/{id}/delegated-writes",
		"PUT /api/workspaces/{id}/jev/credential",
		"PUT /api/workspaces/{id}/mcp-servers/{serverId}",
		"PUT /api/workspaces/{id}/plugins/{installationId}/config",
		"PUT /api/workspaces/{id}/plugins/{installationId}/mcp/{hookKey}/tools",
		"PUT /api/workspaces/{id}/protected-path-policies/{name}",
		"PUT /api/workspaces/{id}/qoder",
		"PUT /api/workspaces/{id}/runtime-profiles/{profileId}",
	} {
		auditPendingL5b[route] = true
	}
}

func TestAdminMutatingRoutesHaveAuditKind(t *testing.T) {
	router := NewRouter(nil, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	seen := map[string]bool{}
	var unclassified []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return nil
		}
		parts := strings.Split(route, "/")
		if len(parts) < 3 || parts[1] != "api" || !auditAdminPrefixes[parts[2]] {
			return nil
		}
		key := method + " " + route
		seen[key] = true
		_, audited := auditedRoutes[key]
		_, exempt := auditExemptRoutes[key]
		if !audited && !exempt && !auditPendingL5b[key] {
			unclassified = append(unclassified, key)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("mutating admin routes with no audit kind, exempt reason or L5b pending entry:\n  %s", strings.Join(unclassified, "\n  "))
	}

	var stale []string
	for key, kind := range auditedRoutes {
		if !seen[key] {
			stale = append(stale, key+" (audited, route gone)")
		}
		if !handler.AuditKindRegistered(kind) {
			stale = append(stale, key+" (kind "+kind+" not registered)")
		}
	}
	for key := range auditExemptRoutes {
		if !seen[key] {
			stale = append(stale, key+" (exempt, route gone)")
		}
	}
	for key := range auditPendingL5b {
		switch {
		case !seen[key]:
			stale = append(stale, key+" (pending, route gone)")
		case auditedRoutes[key] != "" || auditExemptRoutes[key] != "":
			stale = append(stale, key+" (pending, but already audited or exempt)")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("stale route guard entries:\n  %s", strings.Join(stale, "\n  "))
	}
}
