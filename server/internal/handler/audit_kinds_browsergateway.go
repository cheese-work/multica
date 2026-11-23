package handler

import "github.com/multica-ai/multica/server/internal/browserlease"

// Audit kinds for per-gateway browser credentials (CHE-8883, A1b of plan CHE-8829 r16). The row
// names the gateway, the workspace and the credential id (the target). The plaintext and its hash
// are not allowlisted and are never passed in, so neither can reach the row.
func init() {
	spec := auditKindSpec{TargetType: "browser_gateway_credential", Allow: []string{"gateway", "workspace", "revoked"}}
	for _, kind := range []string{browserlease.AuditCredentialIssue, browserlease.AuditCredentialRevoke} {
		auditKinds[kind] = spec
	}
}
