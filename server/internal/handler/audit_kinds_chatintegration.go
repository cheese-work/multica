package handler

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/lark"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for chat-integration installs, uninstalls and DingTalk group
// forget (CHE-5650). The provider is part of the kind, so a row names it
// without storing it twice. Rows carry the installation id (the target), the
// agent and the status, never the config column: it holds the sealed bot
// token, app token or secret.
func init() {
	installation := auditKindSpec{
		TargetType: "channel_installation",
		Allow:      []string{"provider", "agent_id", "status"},
	}
	for _, provider := range []string{"slack", "telegram", "lark", "wecom", "dingtalk"} {
		auditKinds[provider+".install"] = installation
		auditKinds[provider+".uninstall"] = installation
	}
	// The conversation id is a DingTalk identifier, not a credential. The group
	// title is a user-visible name and stays out of the row.
	auditKinds["dingtalk.group.forget"] = auditKindSpec{
		TargetType: "channel_installation",
		Allow:      []string{"conversation_id"},
	}
}

// chatInstallationAuditFields snapshots an installation for audit. The config
// column is deliberately not read.
func chatInstallationAuditFields(provider string, inst db.ChannelInstallation) map[string]any {
	return map[string]any{
		"provider": provider,
		"agent_id": uuidToString(inst.AgentID),
		"status":   inst.Status,
	}
}

// chatInstallAuditHook returns the in-transaction hook an install service runs
// before it commits. It wraps a failed write in auditWriteError so the handler
// answers 500 + Retry-After instead of its provider-specific error mapping.
func (h *Handler) chatInstallAuditHook(r *http.Request, provider string) func(pgx.Tx, db.ChannelInstallation) error {
	return func(tx pgx.Tx, inst db.ChannelInstallation) error {
		err := h.writeAudit(r.Context(), h.Queries.WithTx(tx), r, auditInput{
			Kind: provider + ".install", WorkspaceID: inst.WorkspaceID, TargetID: inst.ID,
			After: chatInstallationAuditFields(provider, inst),
		})
		if err != nil {
			return auditWriteError{err}
		}
		return nil
	}
}

// LarkInstallAuditHook is the in-transaction audit hook for a Lark device-flow
// install. The flow finishes on a background goroutine after the begin request
// has returned, so the actor is the member who began it, recorded on the
// installation as its installer.
func (h *Handler) LarkInstallAuditHook() func(context.Context, pgx.Tx, lark.Installation) error {
	return func(ctx context.Context, tx pgx.Tx, inst lark.Installation) error {
		actor := auditActor{Type: "member", ID: inst.InstallerUserID}
		err := h.writeAudit(ctx, h.Queries.WithTx(tx), nil, auditInput{
			Kind: "lark.install", WorkspaceID: inst.WorkspaceID, TargetID: inst.ID, Actor: &actor,
			After: map[string]any{
				"provider": "lark",
				"agent_id": uuidToString(inst.AgentID),
				"status":   inst.Status,
			},
		})
		if err != nil {
			return auditWriteError{err}
		}
		return nil
	}
}

// revokeChatInstallation flips an installation to revoked and writes the
// uninstall audit row in one transaction. The caller has already authorized the
// request. It answers the failure itself and returns false.
func (h *Handler) revokeChatInstallation(w http.ResponseWriter, r *http.Request, provider, channelType string, wsUUID, instUUID pgtype.UUID) bool {
	ctx := r.Context()
	kind := provider + ".uninstall"
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return false
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)

	before, err := qtx.GetChannelInstallationInWorkspace(ctx, db.GetChannelInstallationInWorkspaceParams{
		ID: instUUID, WorkspaceID: wsUUID, ChannelType: channelType,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return false
	}
	if err := qtx.SetChannelInstallationStatus(ctx, db.SetChannelInstallationStatusParams{
		ID: instUUID, Status: "revoked",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return false
	}
	after := before
	after.Status = "revoked"
	if err := h.writeAudit(ctx, qtx, r, auditInput{
		Kind: kind, WorkspaceID: wsUUID, TargetID: instUUID,
		Before: chatInstallationAuditFields(provider, before),
		After:  chatInstallationAuditFields(provider, after),
	}); err != nil {
		writeAuditFailure(w, r, err, kind)
		return false
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return false
	}
	return true
}
