package handler

import (
	"encoding/json"
	"fmt"
)

var errBlockedRunSkipShape = fmt.Errorf(`blocked_run_skip must be an object {"mode": "off"|"warn"|"block"}`)

// validateBlockedRunSkipPatch checks the blocked_run_skip request field: an
// object holding exactly a mode of off, warn or block (CHE-6973). The read side
// is blockedRunSkipMode.
func validateBlockedRunSkipPatch(raw json.RawMessage) error {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || len(patch) != 1 {
		return errBlockedRunSkipShape
	}
	mode, _ := patch["mode"].(string)
	if mode != "off" && mode != "warn" && mode != "block" {
		return errBlockedRunSkipShape
	}
	return nil
}
