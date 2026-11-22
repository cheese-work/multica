package decisiontx

import (
	"encoding/json"
	"fmt"
)

// SettingCardThread is decision_card_thread (CHE-6431, thread-focus plan T3):
// under "on" a new decision card is one thread that mentions the asked person.
const SettingCardThread = "decision_card_thread"

// CardThreadOn reads decision_card_thread (off or on; default off) from a
// workspace.settings value. A malformed value is an error: callers must not guess.
func CardThreadOn(raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return false, fmt.Errorf("workspace settings: %w", err)
	}
	value, ok := settings[SettingCardThread]
	if !ok {
		return false, nil
	}
	var mode *string // null is not a mode
	if err := json.Unmarshal(value, &mode); err != nil || mode == nil || *mode != "off" && *mode != "on" {
		return false, fmt.Errorf("%s must be off or on", SettingCardThread)
	}
	return *mode == "on", nil
}
