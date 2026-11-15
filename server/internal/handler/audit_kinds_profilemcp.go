package handler

import (
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for runtime profiles, workspace MCP servers, agent create,
// runtime rename and the Qoder connection (CHE-2774 L5a). Anything not in an
// Allow list is stored as the changed marker, so descriptions, fixed args and
// MCP entries (commands, URLs, headers, env) never reach the audit row.
func init() {
	profile := auditKindSpec{
		TargetType: "runtime_profile",
		Allow:      []string{"display_name", "runtime_type", "protocol_family", "command_name", "visibility", "enabled", "runtimes_removed"},
	}
	for _, kind := range []string{"runtime_profile.create", "runtime_profile.update", "runtime_profile.delete"} {
		auditKinds[kind] = profile
	}

	mcp := auditKindSpec{TargetType: "workspace_mcp_server", Allow: []string{"name", "transport"}}
	for _, kind := range []string{"mcp_server.create", "mcp_server.update", "mcp_server.delete"} {
		auditKinds[kind] = mcp
	}

	// Same fields as agent.update; a create is the same snapshot against nothing.
	update := auditKinds["agent.update"]
	auditKinds["agent.create"] = auditKindSpec{TargetType: "agent", Allow: update.Allow, Text: update.Text}

	auditKinds["runtime.rename"] = auditKindSpec{TargetType: "runtime", Allow: []string{"custom_name", "runtimes_renamed"}}

	qoder := auditKindSpec{TargetType: "qoder_connection", Allow: []string{"enabled", "status", "runtimes_removed"}}
	for _, kind := range []string{"qoder.stop", "qoder.disconnect"} {
		auditKinds[kind] = qoder
	}
}

// runtimeProfileAuditFields snapshots a profile. Description and fixed args
// are listed only so a change shows up as a diff; neither is allowlisted.
func runtimeProfileAuditFields(p db.RuntimeProfile) map[string]any {
	return map[string]any{
		"display_name":    p.DisplayName,
		"runtime_type":    p.RuntimeType,
		"protocol_family": p.ProtocolFamily,
		"command_name":    p.CommandName,
		"visibility":      p.Visibility,
		"enabled":         p.Enabled,
		"description":     auditText(p.Description),
		"fixed_args":      string(p.FixedArgs),
	}
}

// workspaceMcpServerAuditFields snapshots a library entry. The config is in
// the snapshot only for the diff; the transport is the one non-secret
// summary of it.
func workspaceMcpServerAuditFields(s db.WorkspaceMcpServer) map[string]any {
	return map[string]any{
		"name":      s.Name,
		"transport": mcpTransportOf(s.Config),
		"config":    string(s.Config),
	}
}
