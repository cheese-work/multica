package handler

import (
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for VCS integrations (CHE-4807 E7): the GitHub App installation
// binding and self-hosted Git provider connections. No token, webhook secret or
// signing secret is allowlisted: a secret change stores the marker "changed".
func init() {
	auditKinds["github.installation.delete"] = auditKindSpec{
		TargetType: "github_installation",
		Allow:      []string{"provider", "installation_id", "account_login", "account_type"},
	}
	connection := auditKindSpec{
		TargetType: "vcs_connection",
		Allow:      []string{"provider", "instance_url", "account_login"},
	}
	for _, kind := range []string{"vcs.connection.create", "vcs.connection.delete", "vcs.webhook.rotate"} {
		auditKinds[kind] = connection
	}
}

// githubInstallationAuditFields snapshots a GitHub installation binding.
func githubInstallationAuditFields(i db.GithubInstallation) map[string]any {
	return map[string]any{
		"provider":        "github",
		"installation_id": i.InstallationID,
		"account_login":   i.AccountLogin,
		"account_type":    i.AccountType,
	}
}

// vcsConnectionAuditFields snapshots a connection. The token and webhook secret
// are placeholders so a replaced or rotated secret shows as "changed" without
// the value (or its ciphertext) ever reaching the audit input.
func vcsConnectionAuditFields(c db.VcsConnection, secretState string) map[string]any {
	return map[string]any{
		"provider":       c.Provider,
		"instance_url":   c.InstanceUrl,
		"account_login":  c.AccountLogin,
		"access_token":   secretState,
		"webhook_secret": secretState,
	}
}
