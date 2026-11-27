package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// reconcilePluginGuardMetadataKeys makes the declared-key projection match the
// consented manifest inside the caller's installation/upgrade transaction. A
// disabled installation keeps its rows with enabled = false, so enabling it
// again needs no manifest read.
func (s *PluginService) reconcilePluginGuardMetadataKeys(
	ctx context.Context,
	queries *db.Queries,
	installation db.PluginInstallation,
	manifest plugincontract.Manifest,
) error {
	existingRows, err := queries.ListPluginGuardMetadataKeysByInstallation(ctx, installation.ID)
	if err != nil {
		return &PluginError{Kind: PluginErrorUnavailable, Message: "list plugin guard metadata keys", Err: err}
	}
	existing := make(map[string]db.PluginGuardMetadataKey, len(existingRows))
	for _, row := range existingRows {
		existing[row.MetadataKey] = row
	}

	declared := make(map[string]struct{})
	for _, hook := range manifest.Contributes.Hooks {
		if hook.Guard == nil || !HookAllowsTrigger(hook, plugincontract.TriggerGuard) {
			continue
		}
		for _, key := range hook.Guard.MetadataWriters {
			// Two hooks (or one hook twice) may declare the same key; the
			// unique index allows one row per installation and key.
			if _, dup := declared[key]; dup {
				continue
			}
			declared[key] = struct{}{}
			row, found := existing[key]
			if !found {
				if _, err := queries.CreatePluginGuardMetadataKey(ctx, db.CreatePluginGuardMetadataKeyParams{
					InstallationID: installation.ID,
					WorkspaceID:    installation.WorkspaceID,
					MetadataKey:    key,
					Enabled:        installation.Enabled,
				}); err != nil {
					return &PluginError{Kind: PluginErrorUnavailable, Message: "create plugin guard metadata key", Err: err}
				}
				continue
			}
			delete(existing, key)
			if err := queries.SetPluginGuardMetadataKeyEnabled(ctx, db.SetPluginGuardMetadataKeyEnabledParams{
				ID: row.ID, Enabled: installation.Enabled,
			}); err != nil {
				return &PluginError{Kind: PluginErrorUnavailable, Message: "update plugin guard metadata key", Err: err}
			}
		}
	}
	for _, orphan := range existing {
		if err := queries.DeletePluginGuardMetadataKey(ctx, orphan.ID); err != nil {
			return &PluginError{Kind: PluginErrorUnavailable, Message: "delete removed plugin guard metadata key", Err: err}
		}
	}
	return nil
}

func (s *PluginService) setPluginGuardMetadataKeysEnabled(
	ctx context.Context,
	queries *db.Queries,
	installationID pgtype.UUID,
	enabled bool,
) error {
	if err := queries.SetPluginGuardMetadataKeysEnabled(ctx, db.SetPluginGuardMetadataKeysEnabledParams{
		InstallationID: installationID, Enabled: enabled,
	}); err != nil {
		return &PluginError{Kind: PluginErrorUnavailable, Message: "update plugin guard metadata keys", Err: err}
	}
	return nil
}
