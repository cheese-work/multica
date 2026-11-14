package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for workspace plugins (CHE-5649). The audit row is written
// through the service's own transaction (service.PluginAuditFn), so a failed
// insert rolls back the mutation, secrets included.
//
// Nothing secret is recorded. Config values reach the row only as the changed
// marker (no config key is allowlisted), install tokens only as the words
// set/unset/rotated/revoked, and no field holds a token or secret value.
func init() {
	installation := auditKindSpec{
		TargetType: "plugin_installation",
		Allow:      []string{"enabled", "version", "granted_scopes", "token", "hook", "tools"},
	}
	for _, kind := range []string{
		"plugin.install", "plugin.uninstall", "plugin.enable", "plugin.disable",
		"plugin.token.rotate", "plugin.token.revoke", "plugin.mcp_tools.approve",
	} {
		auditKinds[kind] = installation
	}
	// Config keys are admin-defined by the plugin manifest, so they can collide
	// with any field name above. No field is allowlisted: every submitted key
	// is stored as its name plus the changed marker.
	auditKinds["plugin.config.update"] = auditKindSpec{TargetType: "plugin_installation"}
	pkg := auditKindSpec{TargetType: "plugin_package", Allow: []string{"plugin_key", "version"}}
	for _, kind := range []string{"plugin.package.publish", "plugin.package.delete"} {
		auditKinds[kind] = pkg
	}
}

// pluginAudit adapts auditInput to a service.PluginAuditFn. Both ends of the
// change are optional (first install has no before, uninstall no after);
// target picks the row's target id from whichever side exists.
func (h *Handler) pluginAudit(r *http.Request, workspaceID pgtype.UUID, kind string,
	fields func(service.PluginChange) (before, after map[string]any),
) service.PluginAuditFn {
	return func(ctx context.Context, q *db.Queries, change service.PluginChange) error {
		before, after := fields(change)
		in := auditInput{Kind: kind, WorkspaceID: workspaceID, Before: before, After: after}
		switch {
		case change.After != nil:
			in.TargetID = change.After.ID
		case change.Before != nil:
			in.TargetID = change.Before.ID
		case change.Package != nil:
			in.TargetID = change.Package.ID
		}
		if err := h.writeAudit(ctx, q, r, in); err != nil {
			return auditWriteError{err}
		}
		return nil
	}
}

func pluginInstallationFields(i *db.PluginInstallation) map[string]any {
	if i == nil {
		return nil
	}
	return map[string]any{
		"enabled":        i.Enabled,
		"version":        i.Version,
		"granted_scopes": pluginGrantedScopes(i.GrantedScopes),
	}
}

func pluginGrantedScopes(raw []byte) string {
	var scopes []string
	_ = json.Unmarshal(raw, &scopes)
	sort.Strings(scopes)
	return strings.Join(scopes, ",")
}

func pluginTokenState(i *db.PluginInstallation) string {
	if i != nil && i.TokenHash.Valid {
		return "set"
	}
	return "unset"
}

// pluginConfigAudit lists the submitted keys only. Secret keys carry the word
// submitted or cleared, never the value; plain values are default-denied to the
// changed marker by the kind's spec.
func pluginConfigAudit(values map[string]any) func(service.PluginChange) (map[string]any, map[string]any) {
	return func(c service.PluginChange) (map[string]any, map[string]any) {
		stored := map[string]any{}
		if c.Before != nil {
			_ = json.Unmarshal(c.Before.Config, &stored)
		}
		before, after := map[string]any{}, map[string]any{}
		for key, value := range values {
			if old, ok := stored[key]; ok {
				before[key] = old
			}
			after[key] = value
		}
		return before, after
	}
}

// pluginApprovedTools reads the approved tool names for one hook from the
// stored approvals JSON (hook key -> {tools:[{name}]}).
func pluginApprovedTools(raw []byte, hookKey string) string {
	var approvals map[string]struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(raw, &approvals)
	names := []string{}
	for _, tool := range approvals[hookKey].Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
