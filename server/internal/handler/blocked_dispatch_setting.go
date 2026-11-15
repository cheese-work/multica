package handler

import (
	"encoding/json"
	"fmt"
)

// blocked_dispatch_dedup is workspace-scoped and defaults to block so an
// unchanged blocked re-entry does not enqueue a duplicate Tofu run.
func validateBlockedDispatchDedupSetting(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("settings must be an object")
	}
	encoded, ok := settings["blocked_dispatch_dedup"]
	if !ok {
		return nil
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &patch); err != nil || patch == nil {
		return fmt.Errorf("blocked_dispatch_dedup must be an object with mode off, warn, or block")
	}
	for key := range patch {
		if key != "mode" {
			return fmt.Errorf("blocked_dispatch_dedup.%s is not settable; use mode", key)
		}
	}
	if rawMode, exists := patch["mode"]; exists {
		var mode string
		if err := json.Unmarshal(rawMode, &mode); err != nil || (mode != "off" && mode != "warn" && mode != "block") {
			return fmt.Errorf("blocked_dispatch_dedup.mode must be off, warn, or block")
		}
	}
	return nil
}
