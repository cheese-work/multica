package service

import (
	"encoding/json"
	"strings"
)

// workspaceAliasWrite is the alias part of a replacing write to a workspace
// built-in: the patch with every alias the patch leaves out cleared, since a
// replace keeps nothing the caller did not send. It is pure, so a preview and
// the write derive the same thing. The legacy instruction limit applies.
func workspaceAliasWrite(settings []byte, key string, patch WakeupConfigPatch) (WakeupConfigPatch, error) {
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
	merged := map[string]json.RawMessage{}
	_ = json.Unmarshal(settings, &merged)
	for k, v := range aliasSettingsPatch(key, alias) {
		raw, err := json.Marshal(v)
		if err != nil {
			return patch, err
		}
		merged[k] = raw
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return patch, err
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
