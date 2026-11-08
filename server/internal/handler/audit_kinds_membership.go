package handler

import (
	"slices"
	"strings"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for membership, invitations, share links and workspace-level
// settings (CHE-2774 L5a).
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
	auditKinds["workspace.export_privacy.update"] = auditKindSpec{
		TargetType: "workspace",
		Allow:      []string{"export_redaction_mode", "export_manifest_retention_days"},
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

func exportPrivacyAuditFields(mode string, retentionDays int32) map[string]any {
	return map[string]any{"export_redaction_mode": mode, "export_manifest_retention_days": retentionDays}
}
