package handler

import (
	"encoding/json"
	"fmt"
)

var errTimeWakeupSkipShape = fmt.Errorf(`time_wakeup_skip must be an object {"mode": "off"|"warn"|"block"}`)

// validateTimeWakeupSkipPatch checks the time_wakeup_skip request field: an
// object holding exactly a mode of off, warn or block (CHE-5987). The read
// side is service.timeWakeupSkipMode; "off" stored reads back as off.
func validateTimeWakeupSkipPatch(raw json.RawMessage) error {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || len(patch) != 1 {
		return errTimeWakeupSkipShape
	}
	mode, _ := patch["mode"].(string)
	if mode != "off" && mode != "warn" && mode != "block" {
		return errTimeWakeupSkipShape
	}
	return nil
}
