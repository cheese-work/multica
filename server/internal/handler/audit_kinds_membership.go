package handler

import (
	"slices"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/governance"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for membership, invitations, share links and workspace-level
// governance settings (CHE-2774 L5a).
func init() {
	// The invitee email is personal data and is not snapshotted at all.
	invitation := auditKindSpec{
		TargetType: "invitation",
		Allow:      []string{"role", "status", "inviter_id", "invitee_user_id", "expires_at"},
	}
	for _, kind := range []string{"invitation.create", "invitation.revoke", "invitation.accept", "invitation.decline"} {
		auditKinds[kind] = invitation
	}
	member := auditKindSpec{TargetType: "member", Allow: []string{"user_id", "role"}}
	for _, kind := range []string{"member.update", "member.remove", "member.leave"} {
		auditKinds[kind] = member
	}
	// The link code is a bearer secret and is never snapshotted.
	auditKinds["share_link.create"] = auditKindSpec{
		TargetType: "share_link",
		Allow:      []string{"role", "max_uses", "use_count", "expires_at", "is_active", "created_by"},
	}
	auditKinds["workspace.delegated_writes.update"] = auditKindSpec{
		TargetType: "workspace",
		Allow:      []string{"allowed_agents", "allowed_fields", "requires_human_decision"},
	}
	// jev_api_url is not allowlisted: a URL can carry credentials.
	auditKinds["governance.config.update"] = auditKindSpec{
		TargetType: "workspace",
		Allow: []string{
			"config_version", "jev_governance_enabled", "jev_fallback_agents_enabled",
			"governance_corrections_enabled", "rule_mode", "jev_squad_routing_enabled",
			"jev_squad_routing_mode", "jev_min_confidence", "max_refreshes", "max_evaluations",
			"admission_window_seconds", "workspace_spend_cap_micro_usd", "pricing_policy_version",
			"input_micro_usd_per_million_tokens", "output_micro_usd_per_million_tokens",
		},
	}
	auditKinds["workspace.export_privacy.update"] = auditKindSpec{
		TargetType: "workspace",
		Allow:      []string{"export_redaction_mode", "export_manifest_retention_days"},
	}
	// A credential row records that it was set, rotated or deleted and nothing
	// else: no field is allowed, so no value or fingerprint can reach the row.
	for _, kind := range []string{"jev.credential.set", "jev.credential.rotate", "jev.credential.delete"} {
		auditKinds[kind] = auditKindSpec{TargetType: "workspace"}
	}
}

// invitationAuditFields snapshots an invitation without the invitee email.
func invitationAuditFields(inv db.WorkspaceInvitation) map[string]any {
	return map[string]any{
		"role":            inv.Role,
		"status":          inv.Status,
		"inviter_id":      uuidToString(inv.InviterID),
		"invitee_user_id": uuidToString(inv.InviteeUserID),
		"expires_at":      inv.ExpiresAt.Time.UTC().Format(time.RFC3339),
	}
}

func memberAuditFields(m db.Member) map[string]any {
	return map[string]any{"user_id": uuidToString(m.UserID), "role": m.Role}
}

// shareLinkAuditFields snapshots a share link without its code.
func shareLinkAuditFields(l db.WorkspaceShareLink) map[string]any {
	fields := map[string]any{
		"role":       l.Role,
		"use_count":  l.UseCount,
		"is_active":  l.IsActive,
		"created_by": uuidToString(l.CreatedBy),
		"max_uses":   nil,
		"expires_at": nil,
	}
	if l.MaxUses.Valid {
		fields["max_uses"] = l.MaxUses.Int32
	}
	if l.ExpiresAt.Valid {
		fields["expires_at"] = l.ExpiresAt.Time.UTC().Format(time.RFC3339)
	}
	return fields
}

// delegatedWritesAuditFields joins the lists into sorted comma strings: agent
// ids and field names are not secret, and a list would otherwise be stored as
// the changed marker.
func delegatedWritesAuditFields(c DelegatedWritesConfig) map[string]any {
	return map[string]any{
		"allowed_agents":          sortedJoin(c.AllowedAgents),
		"allowed_fields":          sortedJoin(c.AllowedFields),
		"requires_human_decision": c.RequiresHumanDecision,
	}
}

func sortedJoin(items []string) string {
	sorted := slices.Clone(items)
	slices.Sort(sorted)
	return strings.Join(sorted, ",")
}

// governanceSettingsAuditFields flattens the workspace governance settings and
// the config version into scalars.
func governanceSettingsAuditFields(s governance.WorkspaceSettings, version int64) map[string]any {
	fields := map[string]any{
		"config_version":                 version,
		"jev_governance_enabled":         s.JevGovernanceEnabled,
		"jev_fallback_agents_enabled":    s.JevFallbackAgentsEnabled,
		"governance_corrections_enabled": s.GovernanceCorrectionsEnabled,
		"rule_mode":                      string(s.RuleMode),
		"jev_squad_routing_enabled":      s.JevSquadRoutingEnabled,
		"jev_squad_routing_mode":         string(s.JevSquadRoutingMode),
		"jev_api_url":                    s.JevAPIURL,
		"jev_min_confidence":             nil,
		"max_refreshes":                  derefInt64(s.Limits.MaxRefreshes),
		"max_evaluations":                derefInt64(s.Limits.MaxEvaluations),
		"admission_window_seconds":       derefInt64(s.Limits.AdmissionWindowSeconds),
		"workspace_spend_cap_micro_usd":  derefInt64(s.Limits.WorkspaceSpendCapMicroUSD),
	}
	if s.JevMinConfidence != nil {
		fields["jev_min_confidence"] = *s.JevMinConfidence
	}
	if p := s.Limits.PricingPolicy; p != nil {
		fields["pricing_policy_version"] = p.Version
		fields["input_micro_usd_per_million_tokens"] = derefInt64(p.InputMicroUSDPerMillionTokens)
		fields["output_micro_usd_per_million_tokens"] = derefInt64(p.OutputMicroUSDPerMillionTokens)
	}
	return fields
}

func derefInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func exportPrivacyAuditFields(mode string, retentionDays int32) map[string]any {
	return map[string]any{"export_redaction_mode": mode, "export_manifest_retention_days": retentionDays}
}

// jevCredentialAuditFields carries only presence; the kind allows no field, so
// the stored row holds the marker and never a value.
func jevCredentialAuditFields(present bool) map[string]any {
	return map[string]any{"present": present}
}
