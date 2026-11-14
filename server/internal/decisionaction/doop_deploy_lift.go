package decisionaction

import (
	"encoding/json"
	"regexp"
)

// DoopDeployLiftParams is the params of doop.deploy_lift: the train marker the
// lift waits on and the repository it lifts. The action changes no Multica row;
// the owner's passkey approval is the whole effect, read back by CI.
type DoopDeployLiftParams struct {
	MarkerValue string `json:"marker_value"`
	Repo        string `json:"repo"`
}

var (
	deployLiftMarkerPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@+=-]{1,128}$`)
	deployLiftRepoPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
)

func normalizeDoopDeployLift(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("doop.deploy_lift takes no secret slots")
	}
	var params DoopDeployLiftParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	if !deployLiftMarkerPattern.MatchString(params.MarkerValue) {
		return nil, nil, invalid("marker_value must be 1 to 128 characters of letters, digits and . _ : / @ + = -")
	}
	if !deployLiftRepoPattern.MatchString(params.Repo) {
		return nil, nil, invalid("repo must be an owner/name slug")
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

// The lift is its own inverse: it changes nothing, so undoing it changes
// nothing either. The owner's revoke (passkey) is what invalidates an approval
// that has not been used yet.
func init() {
	Register(&Definition{Type: "doop.deploy_lift", Version: 1, Passkey: PasskeyRequired, Template: Template{Title: "Lift the doop deploy train", Summary: "Lets the doop deploy train with the proposed marker go ahead. Nothing else changes."}, MinClass: "deploy", Target: TargetNone, OwnerOnly: true, Inverse: "doop.deploy_lift", Normalize: normalizeDoopDeployLift})
}
