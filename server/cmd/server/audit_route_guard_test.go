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
// an admin prefix must be audited, exempt with a reason, or listed as out of
// scope with a reason. Adding an admin route without one of these fails the
// test; so does leaving a stale entry behind once a route is renamed or
// removed. Never grow auditPendingOutOfScope to make the test pass: the
// ratchet in audit_route_guard_scope_test.go fails when it rises.

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
	"POST /api/runtimes/{runtimeId}/revoke-workspace-access":   "runtime.access.revoke",
}

// auditExemptRoutes need no audit row, with the reason.
var auditExemptRoutes = map[string]string{
	"DELETE /api/workspaces/{id}/": "workspace deletion removes the workspace's audit rows in the same tx (audit_workspace_delete)",
}

// auditPendingL5b lists in-scope admin routes whose audit kind is not wired
// yet. It must end empty: TestInScopeAuditRoutesAreWired fails while any entry
// is left. The wired_*_test.go files delete their routes from it.
var auditPendingL5b = map[string]bool{}

// auditPendingOutOfScope lists admin routes deliberately left unaudited, with
// one reason per route. The count may not rise (TestOutOfScopeAuditRatchet) and
// no entry may match an in-scope matcher (TestOutOfScopeAuditRoutesAreNotInScope).
var auditPendingOutOfScope = map[string]string{}

func init() {
	for _, g := range []struct {
		reason string
		routes []string
	}{
		{
			reason: "cloud runtime node lifecycle: operates a hosted VM for the caller, touches no workspace setting, credential or agent grant; the hosting provider keeps its own log (borderline, flagged in the PR)",
			routes: []string{
				"DELETE /api/cloud-runtime/nodes",
				"POST /api/cloud-runtime/nodes",
				"POST /api/cloud-runtime/nodes/exec",
				"POST /api/cloud-runtime/nodes/reboot",
				"POST /api/cloud-runtime/nodes/start",
				"POST /api/cloud-runtime/nodes/status",
				"POST /api/cloud-runtime/nodes/stop",
			},
		},
		{
			reason: "billing: opens a payment-provider session or previews/reconciles seats; the payment provider holds the ledger and no workspace setting or agent reach changes here",
			routes: []string{
				"POST /api/cloud-billing/checkout-sessions",
				"POST /api/cloud-billing/portal-sessions",
				"POST /api/cloud-subscriptions/checkout-sessions",
				"POST /api/cloud-subscriptions/portal-sessions",
				"POST /api/cloud-subscriptions/seats/purchase-preview",
				"POST /api/cloud-subscriptions/seats/purchases",
				"POST /api/cloud-subscriptions/seats/reconcile",
			},
		},
		{
			reason: "per-user Composio connection: the credential lives at Composio and is scoped to the calling user, not the workspace (borderline, flagged in the PR)",
			routes: []string{
				"POST /api/integrations/composio/connect/init",
				"DELETE /api/integrations/composio/connections/{id}",
			},
		},
		{
			reason: "issue-tracking catalog (statuses, workflows, labels, properties, quick actions): shapes issue data, grants no agent reach and stores no credential; changes show in issue history",
			routes: []string{
				"POST /api/issue-statuses/",
				"PATCH /api/issue-statuses/{id}/",
				"DELETE /api/issue-statuses/{id}/",
				"PATCH /api/issue-statuses/reorder",
				"POST /api/issue-workflows/",
				"PATCH /api/issue-workflows/{id}/",
				"DELETE /api/issue-workflows/{id}/",
				"POST /api/issue-workflows/{id}/brief",
				"PUT /api/issue-workflows/{id}/handoffs/{status}",
				"POST /api/labels/",
				"PUT /api/labels/{id}/",
				"DELETE /api/labels/{id}/",
				"POST /api/properties/",
				"PATCH /api/properties/{id}/",
				"POST /api/quick-actions/",
				"PATCH /api/quick-actions/{id}/",
				"DELETE /api/quick-actions/{id}/",
			},
		},
		{
			reason: "workflow webhook endpoint create/rotate/disable: the endpoint only starts a workflow run for that workflow; it does not choose an agent or hold a workspace credential (borderline, flagged in the PR)",
			routes: []string{
				"POST /api/issue-workflows/{id}/webhooks",
				"POST /api/issue-workflows/{id}/webhooks/{endpointId}/disable",
				"POST /api/issue-workflows/{id}/webhooks/{endpointId}/rotate",
			},
		},
		{
			reason: "per-user notification preference, not a workspace setting",
			routes: []string{
				"PATCH /api/notification-channels/",
			},
		},
		{
			reason: "agent-builder chat session draft; the agent it produces is created through POST /api/agents/, which is audited",
			routes: []string{
				"PATCH /api/agent-builder/sessions/{sessionId}/runtime",
				"POST /api/agent-builder/sessions/",
				"PUT /api/agent-builder/sessions/{sessionId}/draft",
			},
		},
		{
			reason: "plugin data plane: a plugin acts on issues and its own storage with the token an admin granted; the install and token routes are audited",
			routes: []string{
				"DELETE /api/plugin-bridge/v1/storage/{scope}/{key}",
				"PUT /api/plugin-bridge/v1/storage/{scope}/{key}",
				"PATCH /api/plugin-bridge/v1/issues/{issue_ref}",
				"POST /api/plugin-bridge/v1/hooks/{key}",
				"POST /api/plugin-bridge/v1/issues/{issue_ref}/comments",
			},
		},
		{
			reason: "personal access tokens: scoped to the calling user, not the workspace; listed in the user's own token page (borderline, flagged in the PR)",
			routes: []string{
				"POST /api/tokens/",
				"DELETE /api/tokens/{id}",
				"POST /api/tokens/current/renew",
			},
		},
		{
			reason: "wakeup rule definitions: schedule when an existing agent is woken, grant no new reach (borderline, flagged in the PR)",
			routes: []string{
				"PUT /api/system-wakeups/{rule}",
				"POST /api/wakeup-definitions/",
				"POST /api/wakeup-definitions/preview",
				"PUT /api/wakeup-definitions/{rule}",
				"DELETE /api/wakeup-definitions/{rule}",
			},
		},
		{
			reason: "inbound webhook receiver authenticated by its own token or signature; not an admin action",
			routes: []string{
				"POST /api/webhooks/autopilots/{token}",
				"POST /api/webhooks/github",
				"POST /api/webhooks/stripe",
				"POST /api/webhooks/vcs/{connectionId}",
				"POST /api/webhooks/workflows/{endpoint_id}",
			},
		},
		{
			reason: "a member redeems a one-time code to bind their own chat id; no credential is stored and the binding reaches only that member's access (judgment call, flagged in the PR)",
			routes: []string{
				"POST /api/dingtalk/binding/redeem",
				"POST /api/lark/binding/redeem",
				"POST /api/slack/binding/redeem",
				"POST /api/telegram/binding/redeem",
				"POST /api/wecom/binding/redeem",
			},
		},
		{
			reason: "personal background-agent override of the calling member (CHE-1446), not a workspace setting",
			routes: []string{
				"PUT /api/workspaces/{id}/me/background-agents",
				"DELETE /api/workspaces/{id}/me/background-agents/{job}",
			},
		},
	} {
		for _, route := range g.routes {
			auditPendingOutOfScope[route] = g.reason
		}
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
		_, outOfScope := auditPendingOutOfScope[key]
		if !audited && !exempt && !auditPendingL5b[key] && !outOfScope {
			unclassified = append(unclassified, key)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("admin mutating routes: %d audited, %d exempt, %d pending in scope, %d out of scope", len(auditedRoutes), len(auditExemptRoutes), len(auditPendingL5b), len(auditPendingOutOfScope))
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("mutating admin routes with no audit kind, exempt reason, pending entry or out-of-scope reason:\n  %s", strings.Join(unclassified, "\n  "))
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
	for key := range auditPendingOutOfScope {
		switch {
		case !seen[key]:
			stale = append(stale, key+" (out of scope, route gone)")
		case auditedRoutes[key] != "" || auditExemptRoutes[key] != "":
			stale = append(stale, key+" (out of scope, but already audited or exempt)")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("stale route guard entries:\n  %s", strings.Join(stale, "\n  "))
	}
}
