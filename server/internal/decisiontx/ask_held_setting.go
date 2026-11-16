package decisiontx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// SettingHeldMode is the workspace.settings key that holds the mode (CHE-5951).
const SettingHeldMode = "held_setting_ask_mode"

// HeldMode decides what an ask for a setting the workspace already holds does.
type HeldMode string

const (
	HeldModeOff   HeldMode = "off"
	HeldModeWarn  HeldMode = "warn"
	HeldModeBlock HeldMode = "block"
)

const maxSettingsNeeded = 10

var settingKey = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)

func parseHeldMode(s string) (HeldMode, error) {
	switch m := HeldMode(s); m {
	case HeldModeOff, HeldModeWarn, HeldModeBlock:
		return m, nil
	}
	return "", fmt.Errorf("%s must be off, warn or block", SettingHeldMode)
}

// HeldModeFromSettings reads the mode from a workspace.settings value. A missing
// key is off. A malformed value is an error: callers must not guess a mode.
func HeldModeFromSettings(raw []byte) (HeldMode, error) {
	if len(raw) == 0 {
		return HeldModeOff, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", fmt.Errorf("workspace settings: %w", err)
	}
	v, ok := settings[SettingHeldMode]
	if !ok {
		return HeldModeOff, nil
	}
	s, _ := v.(string)
	return parseHeldMode(s)
}

// decodeSettingsNeeded decodes and shape-checks the typed settings_needed field.
func decodeSettingsNeeded(raw json.RawMessage, into *[]string) error {
	if err := decodeStrings("ask.settings_needed", raw, into); err != nil {
		return err
	}
	if len(*into) > maxSettingsNeeded {
		return &AskError{"ask.settings_needed", fmt.Sprintf("takes at most %d keys", maxSettingsNeeded)}
	}
	seen := map[string]bool{}
	for i, key := range *into {
		field := fmt.Sprintf("ask.settings_needed[%d]", i)
		if !settingKey.MatchString(key) {
			return &AskError{field, "must be a setting key of letters, digits, _ . -"}
		}
		lower := strings.ToLower(key)
		if seen[lower] {
			return &AskError{field, "repeats an earlier key"}
		}
		seen[lower] = true
	}
	return nil
}

// heldSetting reports whether a stored setting value counts as held: not null,
// not an empty string, list or object.
func heldSetting(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// CheckHeldSettings refuses (block) or hints (warn) when a key in settings_needed
// is already held by the workspace settings. Only the typed field is read, never
// the prose (CHE-3345). The message names the key and never the stored value.
func (ask Ask) CheckHeldSettings(mode HeldMode, settings []byte) (hints []string, err error) {
	if mode == HeldModeOff || mode == "" || len(ask.SettingsNeeded) == 0 {
		return nil, nil
	}
	var stored map[string]any
	if len(settings) > 0 {
		if err := json.Unmarshal(settings, &stored); err != nil {
			return nil, fmt.Errorf("workspace settings: %w", err)
		}
	}
	lower := make(map[string]any, len(stored))
	for k, v := range stored {
		lower[strings.ToLower(k)] = v
	}
	for i, key := range ask.SettingsNeeded {
		if !heldSetting(lower[strings.ToLower(key)]) {
			continue
		}
		problem := fmt.Sprintf("%q is already set in workspace settings; read it with multica workspace get, do not ask a person", key)
		field := fmt.Sprintf("ask.settings_needed[%d]", i)
		if mode == HeldModeBlock {
			return nil, &AskError{field, problem}
		}
		hints = append(hints, field+" "+problem)
	}
	return hints, nil
}
