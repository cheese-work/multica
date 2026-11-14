package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// workspace.settings.patch (CHE-3480) writes the allow-table keys of one
// workspace's settings (decisionaction.CheckConfigChange). One digest covers
// the named keys, so the patch is one compare-and-swap, and the write is one
// UPDATE under the row lock: a key that fails validation stops the whole patch
// and a stored key the patch does not name keeps its value.

const settingsPatchAction = "workspace.settings.patch"

// settingsPatchParams is the strict shape of the params. Values stay raw so
// the digest covers the exact bytes the agent sent, canonicalised.
type settingsPatchParams struct {
	WorkspaceID string                     `json:"workspace_id"`
	Settings    map[string]json.RawMessage `json:"settings"`
	BaseSHA256  string                     `json:"base_sha256"`
	AfterSHA256 string                     `json:"after_sha256"`
}

func init() {
	decisionaction.Register(&decisionaction.Definition{
		Type: settingsPatchAction, Version: 1, Passkey: decisionaction.PasskeyRequired, MinClass: "spend", Target: decisionaction.TargetWorkspace, RequiresPreview: true,
		Template:  decisionaction.Template{Title: "Change workspace settings", Summary: "Sets the proposed workspace setting keys."},
		Normalize: normalizeSettingsPatch,
	})
	registerConfigChange(settingsPatchAction, configChange{
		Fields: []string{decisionaction.SettingsField},
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			_, err := loadWorkspaceTarget(ctx, queries, workspaceID, params, http.StatusConflict, true)
			return err
		},
		Read:    readSettingsPatch,
		Apply:   applySettingsPatch,
		Publish: publishSettingsPatch,
	})
}

func normalizeSettingsPatch(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, &decisionaction.Error{Code: decisionaction.CodeInvalidParams, Message: settingsPatchAction + " takes no secret slots"}
	}
	var params settingsPatchParams
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return nil, nil, &decisionaction.Error{Code: decisionaction.CodeInvalidParams, Message: fmt.Sprintf("params: %v", err)}
	}
	if len(params.Settings) == 0 {
		return nil, nil, &decisionaction.Error{Code: decisionaction.CodeInvalidParams, Message: "settings must name at least one key"}
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

// settingsPatchKeys is the sorted key list of the patch.
func settingsPatchKeys(patch map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(patch))
	for key := range patch {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// objectMergeSettings keep the stored fields a patch does not name, as the
// ordinary workspace update does for them.
var objectMergeSettings = map[string]bool{"auto_compact": true, "idle_started_guard": true}

var settingsBoolKeys = []string{
	"github_auto_link_prs_enabled", "github_pr_sidebar_enabled", "co_authored_by_enabled",
	"github_wake_on_pr_merge", "github_wake_on_ci_failure", "github_wake_on_ci_success", "github_wake_on_pr_attention",
	"system_wakeup_child_done", "agent_comment_wake_policy_enabled", wakeupsPausedKey,
}

func settingsInvalid(format string, args ...any) error {
	return decisionIntentRefusal{http.StatusBadRequest, "invalid_params: " + fmt.Sprintf(format, args...)}
}

// validateSettingsPatchValue checks one non-null value with the validator the
// ordinary settings write uses, or the type the reader of the key expects.
func validateSettingsPatchValue(key string, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return settingsInvalid("%s is not valid JSON", key)
	}
	var err error
	switch {
	case key == "provider_breaker_max_hold_seconds":
		err = validateProviderHoldSetting(map[string]any{key: value})
	case key == "guard_policy":
		err = validateGuardPolicySetting(map[string]any{key: value})
	case key == "compact_prompt":
		err = validateCompactPromptConfig(map[string]any{key: value})
		if _, isString := value.(string); !isString && err == nil {
			err = fmt.Errorf("compact_prompt must be a string")
		}
	case key == "auto_compact":
		if _, isObject := value.(map[string]any); !isObject {
			return settingsInvalid("auto_compact must be an object")
		}
		err = validateAutoCompactConfig(map[string]any{key: value})
	case key == "idle_started_guard":
		err = validateIdleGuardPatch(raw)
	case key == "system_wakeup_child_done_instruction":
		if text, isString := value.(string); !isString || len(text) > 16384 {
			err = fmt.Errorf("%s must be a string of at most 16384 bytes", key)
		}
	case key == "mermaid_orientation":
		if text, _ := value.(string); text != "auto" && text != "author" {
			err = fmt.Errorf("mermaid_orientation must be auto or author")
		}
	case key == "pr_event_capture":
		flags, isObject := value.(map[string]any)
		if !isObject {
			err = fmt.Errorf("pr_event_capture must be an object of booleans")
		}
		for kind, flag := range flags {
			if _, isBool := flag.(bool); !isBool {
				err = fmt.Errorf("pr_event_capture.%s must be a boolean", kind)
			}
		}
	case slices.Contains(settingsBoolKeys, key):
		if _, isBool := value.(bool); !isBool {
			err = fmt.Errorf("%s must be a boolean", key)
		}
	}
	if err != nil {
		return settingsInvalid("%v", err)
	}
	return nil
}

// settingsPatchAfter returns the named keys as they stand after the patch:
// the patched value, or for an object-merge key the stored object with the
// named fields laid over it. A null removes the key and reads as null.
func settingsPatchAfter(stored, patch map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	after := make(map[string]json.RawMessage, len(patch))
	for key, value := range patch {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			after[key] = nil
			continue
		}
		if err := validateSettingsPatchValue(key, value); err != nil {
			return nil, err
		}
		if key == wakeupsPausedKey {
			// An unpaused workspace stores no switch (reconcileWakeupsPause), so
			// false reads back as unset, the same as null.
			if bytes.Equal(bytes.TrimSpace(value), []byte("true")) {
				after[key] = value
			} else {
				after[key] = nil
			}
			continue
		}
		if !objectMergeSettings[key] {
			after[key] = value
			continue
		}
		merged := map[string]json.RawMessage{}
		if len(stored[key]) > 0 {
			_ = json.Unmarshal(stored[key], &merged) // a stored non-object is replaced by the patch
		}
		var named map[string]json.RawMessage
		_ = json.Unmarshal(value, &named) // validated as an object above
		for field, fieldValue := range named {
			merged[field] = fieldValue
		}
		raw, err := json.Marshal(merged)
		if err != nil {
			return nil, err
		}
		after[key] = raw
	}
	return after, nil
}

func decodeStoredSettings(raw []byte) (map[string]json.RawMessage, error) {
	stored := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return stored, nil
	}
	if err := json.Unmarshal(raw, &stored); err != nil || stored == nil {
		return nil, fmt.Errorf("workspace settings are not an object")
	}
	return stored, nil
}

// namedSettings is the value of each key, nil when the key is unset.
func namedSettings(stored map[string]json.RawMessage, keys []string) map[string]json.RawMessage {
	named := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		named[key] = stored[key]
	}
	return named
}

// settingsView is the JSON the approver reads: a null for an unset key.
func settingsView(named map[string]json.RawMessage) map[string]any {
	view := make(map[string]any, len(named))
	for key, value := range named {
		if len(value) == 0 {
			view[key] = nil
			continue
		}
		view[key] = value
	}
	return view
}

func readSettingsPatch(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, raw json.RawMessage) (configChangeRead, error) {
	var params settingsPatchParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return configChangeRead{}, settingsInvalid("params must be an object")
	}
	keys := settingsPatchKeys(params.Settings)
	read := configChangeRead{Field: decisionaction.SettingsField, Keys: keys, TargetID: uuidToString(workspaceID), HumanBlocksIntact: true}
	if decisionaction.CheckConfigChange(read.Field, keys) != nil {
		return read, nil // the core refuses with the denylist code
	}
	workspace, err := loadWorkspaceTarget(ctx, queries, workspaceID, raw, http.StatusBadRequest, false)
	if err != nil {
		return read, err
	}
	stored, err := decodeStoredSettings(workspace.Settings)
	if err != nil {
		return read, err
	}
	after, err := settingsPatchAfter(stored, params.Settings)
	if err != nil {
		return read, err
	}
	before := namedSettings(stored, keys)
	if read.LiveSHA256, err = decisionaction.SettingsDigest(read.TargetID, before); err != nil {
		return read, err
	}
	if read.AfterSHA256, err = decisionaction.SettingsDigest(read.TargetID, after); err != nil {
		return read, err
	}
	read.Before, read.After = settingsView(before), settingsView(after)
	return read, nil
}

func applySettingsPatch(ctx context.Context, execution decisionActionExecution, read configChangeRead) error {
	var params settingsPatchParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return err
	}
	// The row is locked and its settings are read again here: the patch is laid
	// over what is stored now, so every unnamed key keeps its value.
	workspace, err := loadWorkspaceTarget(ctx, execution.Queries, execution.Action.WorkspaceID, execution.Action.Params, http.StatusConflict, false)
	if err != nil {
		return err
	}
	stored, err := decodeStoredSettings(workspace.Settings)
	if err != nil {
		return err
	}
	after, err := settingsPatchAfter(stored, params.Settings)
	if err != nil {
		return err
	}
	for key, value := range after {
		if value == nil {
			delete(stored, key)
		} else {
			stored[key] = value
		}
	}
	pauseAction := ""
	if _, names := params.Settings[wakeupsPausedKey]; names {
		if pauseAction, err = reconcileSettingsPatchPause(workspace.Settings, stored, uuidToString(execution.Approver.UserID)); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	ws, err := execution.Queries.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: workspace.ID, Settings: encoded})
	if err != nil || pauseAction == "" {
		return err
	}
	if pauseAction == wakeupsResumedAction {
		// The same settle the ordinary resume runs, in the approval transaction.
		if err := (&service.IssueWakeupService{Tasks: &service.TaskService{Queries: execution.Queries}}).SettleWakeupsOnResume(ctx, execution.Tx, ws.ID); err != nil {
			return err
		}
	}
	_, err = execution.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: ws.ID, ActorType: pgtype.Text{String: "member", Valid: true}, ActorID: execution.Approver.UserID,
		Action: pauseAction, Details: []byte(`{}`),
	})
	return err
}

// reconcileSettingsPatchPause lets the pause switch of the patched settings go
// through the ordinary rule: the server stamps who paused and when, a pause of
// a paused workspace keeps its stamp, and a resume removes the switch. It
// rewrites only the pause keys of patched, so every other stored value keeps
// its exact bytes, and returns the activity action of a flip.
func reconcileSettingsPatchPause(storedRaw []byte, patched map[string]json.RawMessage, approverID string) (string, error) {
	pauseKeys := []string{wakeupsPausedKey, wakeupsPausedByKey, wakeupsPausedAtKey, wakeupsResumedAtKey}
	var storedAll map[string]json.RawMessage
	if err := json.Unmarshal(storedRaw, &storedAll); err != nil {
		return "", err
	}
	stored, incoming := map[string]any{}, map[string]any{}
	for _, source := range []struct {
		from map[string]json.RawMessage
		to   map[string]any
	}{{storedAll, stored}, {patched, incoming}} {
		for _, key := range pauseKeys {
			if raw, present := source.from[key]; present {
				var value any
				if err := json.Unmarshal(raw, &value); err != nil {
					return "", err
				}
				source.to[key] = value
			}
		}
	}
	// The patch leaves the pause stamps alone; the rule rewrites them.
	incoming[wakeupsPausedKey] = incoming[wakeupsPausedKey] == true
	action, err := reconcileWakeupsPause(stored, incoming, approverID, time.Now())
	if err != nil {
		return "", err
	}
	for _, key := range pauseKeys {
		value, present := incoming[key]
		if !present {
			delete(patched, key)
			continue
		}
		if patched[key], err = json.Marshal(value); err != nil {
			return "", err
		}
	}
	return action, nil
}

// publishSettingsPatch announces the committed settings to clients and wakes
// every member's daemons, which cache workspace settings, as the ordinary
// settings write does.
func publishSettingsPatch(ctx context.Context, handler *Handler, action db.DecisionAction) error {
	workspace, err := handler.Queries.GetWorkspace(ctx, action.WorkspaceID)
	if err != nil {
		return err
	}
	handler.publish(protocol.EventWorkspaceUpdated, uuidToString(workspace.ID), "agent", uuidToString(action.RequestedByAgentID), map[string]any{"workspace": handler.workspaceToResponse(workspace)})
	members, err := handler.Queries.ListMembers(ctx, workspace.ID)
	if err != nil {
		return err
	}
	userIDs := make([]string, 0, len(members))
	for _, member := range members {
		userIDs = append(userIDs, uuidToString(member.UserID))
	}
	handler.notifyDaemonWorkspacesChanged(userIDs...)
	return nil
}
