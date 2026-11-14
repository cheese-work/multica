package handler

import (
	"errors"
	"net/http"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// CHE-3482: a machine identity never writes a covered field through an older
// route. It is refused with this code and the action type that owns the change.
// The human owner CAS path and project.description do not come through here.
const codePasskeyActionRequired = "passkey_action_required"

const (
	squadInstructionsAction  = "squad.instructions.set"
	workspaceContextAction   = "workspace.context.set"
	workflowStepsAction      = decisionaction.WorkflowStepsSet
	errPasskeyRequiredPrefix = "this change needs a passkey-approved action: "
)

// errSettingsPasskeyRequired is saveWorkspace's refusal of a machine write
// that moves a key on the settings allow table.
var errSettingsPasskeyRequired = errors.New("settings allow-table key written by a machine identity")

func writePasskeyActionRequired(w http.ResponseWriter, actionType string) {
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error":       errPasskeyRequiredPrefix + actionType,
		"code":        codePasskeyActionRequired,
		"action_type": actionType,
	})
}

// refuseMachineCoveredWrite answers 403 passkey_action_required when a machine
// identity writes the covered field. It runs before every older exception
// (Hermes, vote receipt, decision binding, PASS receipt).
func refuseMachineCoveredWrite(w http.ResponseWriter, r *http.Request, actorType string, writing bool, actionType string) bool {
	if !writing || !isGovernedFieldMachineActor(r, actorType) {
		return false
	}
	writePasskeyActionRequired(w, actionType)
	return true
}
