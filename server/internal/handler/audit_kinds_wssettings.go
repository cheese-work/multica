package handler

import (
	"sort"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for the last workspace-settings routes (CHE-5655, E4 of
// CHE-4807). Default-deny: a field outside Allow and Text is stored as the
// changed marker, so no Qoder or GitHub token reaches a row.
func init() {
	auditKinds["workspace.create"] = auditKindSpec{
		TargetType: "workspace",
		Allow:      []string{"name", "slug", "issue_prefix"},
		Text:       []string{"description", "context"},
	}
	auditKinds["workspace.protected_path_policy.update"] = auditKindSpec{
		TargetType: "workspace",
		Allow:      []string{"name", "patterns"},
	}
	// The link code is a bearer secret and is never snapshotted.
	auditKinds["share_link.revoke"] = auditKindSpec{
		TargetType: "share_link",
		Allow:      []string{"role", "max_uses", "use_count", "expires_at", "is_active", "created_by"},
	}
	// qoder_token and github_tokens are in the snapshot only so a rotation shows
	// up as a diff; neither is allowlisted.
	qoder := auditKindSpec{
		TargetType: "qoder_connection",
		Allow:      []string{"name", "base_url", "environment_id", "max_concurrency", "run_timeout_minutes", "enabled", "status"},
	}
	for _, kind := range []string{"qoder.save", "qoder.resume"} {
		auditKinds[kind] = qoder
	}
}

// workspaceCreateAuditFields snapshots a new workspace.
func workspaceCreateAuditFields(ws db.Workspace) map[string]any {
	return map[string]any{
		"name":         ws.Name,
		"slug":         ws.Slug,
		"issue_prefix": ws.IssuePrefix,
		"description":  auditText(ws.Description),
		"context":      auditText(ws.Context),
	}
}

// protectedPathPolicyAuditFields snapshots one named policy. Patterns are
// relative paths, not secrets, so they are joined into a readable string.
func protectedPathPolicyAuditFields(name string, patterns []string) map[string]any {
	return map[string]any{"name": name, "patterns": strings.Join(patterns, ",")}
}

// qoderAuditFields snapshots a Qoder connection. Credentials are present only
// as values the default-deny redaction replaces with the changed marker.
func qoderAuditFields(cfg qoderSettings, enabled bool, status string) map[string]any {
	repos := make([]string, 0, len(cfg.GitHubTokens))
	for repo, token := range cfg.GitHubTokens {
		repos = append(repos, repo+"="+token)
	}
	sort.Strings(repos)
	return map[string]any{
		"name":                cfg.Name,
		"base_url":            cfg.BaseURL,
		"environment_id":      cfg.EnvironmentID,
		"max_concurrency":     cfg.MaxConcurrency,
		"run_timeout_minutes": cfg.RunTimeoutMinutes,
		"enabled":             enabled,
		"status":              status,
		"qoder_token":         cfg.QoderToken,
		"github_tokens":       strings.Join(repos, ","),
	}
}
