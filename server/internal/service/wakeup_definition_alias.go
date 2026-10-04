package service

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// workspaceSettingsObject decodes the workspace settings as the object an alias
// write merges into. The settings column is jsonb and the workspace update
// stores whatever it is given, so an array, a scalar or JSON null can be there;
// a merge into those does not produce the object a simulation would, and a nil
// map cannot be written to. Those shapes are refused with an input error before
// anything is simulated or persisted.
func workspaceSettingsObject(settings []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(settings)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(settings, &object); err != nil || object == nil {
		return nil, wakeupDefinitionBad("the workspace settings are not a JSON object, so an alias field cannot be written; repair them first")
	}
	return object, nil
}

// requireAliasSettings refuses, before the chain is read or anything is
// written, an alias-bearing patch for a workspace whose settings are not an
// object. A patch with no alias field merges nothing and is not refused.
func requireAliasSettings(ctx context.Context, q *db.Queries, ws pgtype.UUID, key string, patch WakeupConfigPatch) error {
	if len(aliasSettingsPatch(key, patch)) == 0 {
		return nil
	}
	row, err := q.GetWorkspace(ctx, ws)
	if err != nil {
		return err
	}
	_, err = workspaceSettingsObject(row.Settings)
	return err
}

// workspaceAliasWrite is the alias part of a replacing write to a workspace
// built-in: the patch with every alias the patch leaves out cleared, since a
// replace keeps nothing the caller did not send. It is pure, so a preview and
// the write derive the same thing. The legacy instruction limit applies.
func workspaceAliasWrite(settings []byte, key string, patch WakeupConfigPatch) (WakeupConfigPatch, error) {
	if len(aliasSettingsPatch(key, patch)) > 0 {
		if _, err := workspaceSettingsObject(settings); err != nil {
			return patch, err
		}
	}
	current, err := legacyWorkspacePatch(settings, key)
	if err != nil {
		return patch, err
	}
	alias := patch
	if current.Enabled.Set && !patch.Enabled.Set {
		alias.Enabled = wakeupField[bool]{Set: true, Null: true}
	}
	if key == SystemRuleChildDone {
		if current.Instruction.Set && !patch.Instruction.Set {
			alias.Instruction = wakeupField[string]{Set: true, Null: true}
		}
		if alias.Instruction.Set && !alias.Instruction.Null && len(alias.Instruction.Value) > MaxSystemWakeupInstruction {
			return patch, wakeupDefinitionBad("this rule's workspace instruction must be at most %d bytes", MaxSystemWakeupInstruction)
		}
	}
	return alias, nil
}

// aliasSettingsPatch is what applying an alias patch merges into the workspace
// settings. It mirrors ApplyWorkspaceWakeupAliases: a null enabled of child_done
// is stored as on, a null PR enabled as JSON null, and an instruction is stored
// trimmed (a null one as empty).
func aliasSettingsPatch(key string, alias WakeupConfigPatch) map[string]any {
	out := map[string]any{}
	switch key {
	case SystemRuleChildDone:
		if alias.Enabled.Set {
			out[WorkspaceSettingChildDone] = alias.Enabled.Null || alias.Enabled.Value
		}
		if alias.Instruction.Set {
			text := ""
			if !alias.Instruction.Null {
				text = strings.TrimSpace(alias.Instruction.Value)
			}
			out[WorkspaceSettingChildDoneInstruction] = text
		}
	case SystemRulePRMerged, SystemRulePRChecksFailed:
		if alias.Enabled.Set {
			setting, _ := prWakeupSetting(key)
			if alias.Enabled.Null {
				out[setting] = nil
			} else {
				out[setting] = alias.Enabled.Value
			}
		}
	}
	return out
}

// workspaceAliasReadback is the patch a workspace built-in reads back after a
// replacing write of patch: the stored fields as sent, the alias fields as the
// settings hold them once the write is applied (a cleared child_done enabled
// reads back as on, a cleared PR enabled as unset). It never persists, so a
// preview can resolve exactly what saving would leave behind.
func workspaceAliasReadback(settings []byte, key string, patch WakeupConfigPatch) (WakeupConfigPatch, error) {
	alias, err := workspaceAliasWrite(settings, key, patch)
	if err != nil {
		return patch, err
	}
	raw := settings
	if sets := aliasSettingsPatch(key, alias); len(sets) > 0 {
		merged, err := workspaceSettingsObject(settings)
		if err != nil {
			return patch, err
		}
		for k, v := range sets {
			encoded, err := json.Marshal(v)
			if err != nil {
				return patch, err
			}
			merged[k] = encoded
		}
		if raw, err = json.Marshal(merged); err != nil {
			return patch, err
		}
	}
	after, err := legacyWorkspacePatch(raw, key)
	if err != nil {
		return patch, err
	}
	out := patch
	out.Enabled = after.Enabled
	if key == SystemRuleChildDone {
		out.Instruction = after.Instruction
	}
	return out, nil
}
