package handler

import (
	"context"
	"encoding/json"
)

const doopDeployLiftAction = "doop.deploy_lift"

// executeDoopDeployLiftAction changes nothing: the owner's passkey approval is
// the whole effect. The doop train lift reads that approval through
// GET /api/decisions/{id}/state.
func executeDoopDeployLiftAction(_ context.Context, _ decisionActionExecution) (json.RawMessage, error) {
	return json.RawMessage(`{"effect":"none"}`), nil
}
