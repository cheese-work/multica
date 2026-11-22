package handler

import (
	"encoding/json"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// validateBlockedRunSkipPatch checks the blocked_run_skip request field: an
// object holding exactly a mode of off, warn or block (CHE-6973). The read side
// is blockedRunSkipMode.
func validateBlockedRunSkipPatch(raw json.RawMessage) error {
	return decisionaction.ValidateModeSetting("blocked_run_skip", raw)
}
