package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The four workspace settings behind the platform rules are canonical legacy
// field aliases, not an import source: github_wake_on_pr_merge and
// github_wake_on_ci_failure are the workspace "enabled" of pr_merged and
// pr_checks_failed, system_wakeup_child_done and
// system_wakeup_child_done_instruction are the "enabled" and "instruction" of
// child_done. Reads project them into a workspace-scope definition, writes go
// through the same settings transaction, and the definition store never holds
// a second copy. github_enabled=false is not an alias; it stays an integration
// veto the legacy PR reader applies at every scope.

// prWakeupSettings is the shape of the workspace settings the PR rules read.
// Decoding it is what makes a malformed value fail closed.
type prWakeupSettings struct {
	GitHubEnabled   *bool `json:"github_enabled"`
	WakeOnPRMerge   *bool `json:"github_wake_on_pr_merge"`
	WakeOnCIFailure *bool `json:"github_wake_on_ci_failure"`
}

// legacyWorkspacePatch projects the aliases of one rule. A missing, null or
// otherwise unrecognized value projects nothing, so the built-in baseline
// (default on) applies exactly as the legacy readers apply it. Malformed PR
// settings are an error, as in PRWakeupEnabled; child_done reads leniently, as
// SystemWakeupDefault does.
func legacyWorkspacePatch(settings []byte, ruleKey string) (WakeupConfigPatch, error) {
	var p WakeupConfigPatch
	switch ruleKey {
	case SystemRulePRMerged, SystemRulePRChecksFailed:
		if len(settings) == 0 {
			return p, nil
		}
		var v prWakeupSettings
		if err := json.Unmarshal(settings, &v); err != nil {
			return p, malformedPRWakeupSettings(err)
		}
		enabled := v.WakeOnPRMerge
		if ruleKey == SystemRulePRChecksFailed {
			enabled = v.WakeOnCIFailure
		}
		if enabled != nil {
			p.Enabled = wakeupField[bool]{Set: true, Value: *enabled}
		}
	case SystemRuleChildDone:
		var values map[string]json.RawMessage
		if json.Unmarshal(settings, &values) != nil {
			return p, nil
		}
		switch string(values[WorkspaceSettingChildDone]) {
		case "false":
			p.Enabled = wakeupField[bool]{Set: true, Value: false}
		case "true":
			p.Enabled = wakeupField[bool]{Set: true, Value: true}
		}
		var instruction string
		_ = json.Unmarshal(values[WorkspaceSettingChildDoneInstruction], &instruction)
		if instruction = strings.TrimSpace(instruction); instruction != "" {
			p.Instruction = wakeupField[string]{Set: true, Value: instruction}
		}
	}
	return p, nil
}

// WorkspaceWakeupDefinition is the workspace-scope definition of a built-in
// rule: the stored definition (nil when none) with the legacy alias fields
// taken from the workspace settings, which win. It returns nil when neither
// exists. stored is never modified.
func WorkspaceWakeupDefinition(workspaceID pgtype.UUID, stored *WakeupDefinition, settings []byte, ruleKey string) (*WakeupDefinition, error) {
	alias, err := legacyWorkspacePatch(settings, ruleKey)
	if err != nil {
		return nil, err
	}
	if !alias.Enabled.Set && !alias.Instruction.Set {
		return stored, nil
	}
	def := WakeupDefinition{Scope: WakeupScopeWorkspace, ScopeID: workspaceID}
	var base int64
	if stored != nil {
		def, base = *stored, stored.Revision
	}
	if alias.Enabled.Set {
		def.Patch.Enabled = alias.Enabled
	}
	if alias.Instruction.Set {
		def.Patch.Instruction = alias.Instruction
	}
	def.Revision = wakeupContentRevision(strconv.FormatInt(base, 10), boolFieldKey(alias.Enabled), alias.Instruction.Value)
	return &def, nil
}

func boolFieldKey(f wakeupField[bool]) string {
	switch {
	case !f.Set:
		return "-"
	case f.Null:
		return "null"
	}
	return strconv.FormatBool(f.Value)
}

// wakeupContentRevision derives a revision from legacy state that carries no
// revision of its own, so a legacy write changes the effective-config
// fingerprint exactly as a new definition write does.
func wakeupContentRevision(parts ...string) int64 {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p)) + ":" + p))
	}
	return int64(binary.BigEndian.Uint64(h.Sum(nil)[:8])>>2) + 1
}

// ApplyWorkspaceWakeupAliases writes the alias fields of a workspace-scope
// patch through the legacy settings path and returns the rest, which belongs in
// the definition store. child_done goes through SetChildDoneDefault, so rules
// nobody customized follow it exactly as with the legacy endpoint; the PR
// settings are read live and only need the settings write. A null clears back
// to the inherited default (on, no instruction). The legacy 4,000-byte
// instruction limit applies to the alias.
func (s *IssueWakeupService) ApplyWorkspaceWakeupAliases(ctx context.Context, workspaceID pgtype.UUID, ruleKey string, p WakeupConfigPatch) (WakeupConfigPatch, error) {
	rest := p
	switch ruleKey {
	case SystemRuleChildDone:
		var enabled *bool
		var instruction *string
		if p.Enabled.Set {
			on := p.Enabled.Null || p.Enabled.Value
			enabled, rest.Enabled = &on, wakeupField[bool]{}
		}
		if p.Instruction.Set {
			text := p.Instruction.Value
			if p.Instruction.Null {
				text = ""
			}
			instruction, rest.Instruction = &text, wakeupField[string]{}
		}
		if _, err := s.SetChildDoneDefault(ctx, workspaceID, enabled, instruction); err != nil {
			return p, err
		}
	case SystemRulePRMerged, SystemRulePRChecksFailed:
		if !p.Enabled.Set {
			return p, nil
		}
		setting, _ := prWakeupSetting(ruleKey)
		var value any
		if !p.Enabled.Null {
			value = p.Enabled.Value
		}
		raw, err := json.Marshal(map[string]any{setting: value})
		if err != nil {
			return p, err
		}
		tx, err := s.Tasks.TxStarter.Begin(ctx)
		if err != nil {
			return p, err
		}
		defer tx.Rollback(ctx)
		if err := s.Tasks.Queries.WithTx(tx).MergeWorkspaceSettings(ctx, db.MergeWorkspaceSettingsParams{ID: workspaceID, Patch: raw}); err != nil {
			return p, err
		}
		if err := tx.Commit(ctx); err != nil {
			return p, err
		}
		rest.Enabled = wakeupField[bool]{}
	}
	return rest, nil
}
