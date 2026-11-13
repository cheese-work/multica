package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5553 (CHE-5474 C-10.6, G-L): doop.deploy_lift v1 is the decision a doop
// train lift waits for. Only the workspace owner's passkey approval answers it.

var deployLiftParams = map[string]any{"marker_value": "train-2026-11-14", "repo": "cheese-work/doop"}

func newPendingDeployLift(test *testing.T, params map[string]any) pendingAction {
	test.Helper()
	fixture := newActionFixture(test)
	body := fixture.withAction(map[string]any{"type": "doop.deploy_lift", "version": 1, "params": params})
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	action := pendingAction{fixture, created.ID, created.Action.ID}
	makeApproverOwner(test, action, "owner")
	return action
}

func TestDeployLiftIsRegisteredAsAPasskeyOwnerOnlyDeployAction(test *testing.T) {
	definition, found := decisionaction.Lookup("doop.deploy_lift", 1)
	if !found {
		test.Fatal("doop.deploy_lift@1 is not registered")
	}
	if !definition.RequiresPasskey() || definition.MinClass != "deploy" || definition.Inverse != "" || definition.Template.Title == "" {
		test.Fatalf("definition = %+v, want passkey, class deploy, no inverse, a template", definition)
	}
	if decisionActionExecutors["doop.deploy_lift"] == nil {
		test.Fatal("doop.deploy_lift has no executor")
	}
	if !actionOwnerOnly(db.DecisionAction{ActionType: "doop.deploy_lift", ActionVersion: 1}) {
		test.Fatal("doop.deploy_lift is not owner-only")
	}
}

func TestDeployLiftRequestStoresItsParamsAndNeedsNoTargetRow(test *testing.T) {
	action := newPendingDeployLift(test, deployLiftParams)
	var raw string
	dbfx.QueryRow(test, `SELECT params::text FROM decision_action WHERE id = $1`, action.actionID).Scan(&raw)
	var params map[string]string
	if err := json.Unmarshal([]byte(raw), &params); err != nil || len(params) != 2 || params["marker_value"] != "train-2026-11-14" || params["repo"] != "cheese-work/doop" {
		test.Fatalf("params = %s (%v)", raw, err)
	}
}

func TestDeployLiftRequestRefusesBadParams(test *testing.T) {
	for name, params := range map[string]map[string]any{
		"no marker":     {"repo": "cheese-work/doop"},
		"no repo":       {"marker_value": "m1"},
		"repo not slug": {"marker_value": "m1", "repo": "doop"},
		"marker spaces": {"marker_value": "a b", "repo": "cheese-work/doop"},
		"extra field":   {"marker_value": "m1", "repo": "cheese-work/doop", "extra": "x"},
	} {
		test.Run(name, func(test *testing.T) {
			fixture := newActionFixture(test)
			fixture.respond(test, fixture.withAction(map[string]any{"type": "doop.deploy_lift", "version": 1, "params": params}), http.StatusBadRequest, nil)
			if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
				test.Fatalf("refused request stored %d requests and %d actions", requests, actions)
			}
		})
	}
}

func TestDeployLiftApprovalNeedsAPasskeyAndTheOwner(test *testing.T) {
	test.Run("no verifier fails closed", func(test *testing.T) {
		action := newPendingDeployLift(test, deployLiftParams)
		if body := action.approve(test, testHandler, action.claim(test, nil), http.StatusForbidden); !strings.Contains(body, "approval_proof_unavailable") {
			test.Fatalf("body %q", body)
		}
		action.assertUntouched(test)
	})
	test.Run("an admin is refused", func(test *testing.T) {
		action := newPendingDeployLift(test, deployLiftParams)
		makeApproverOwner(test, action, "admin")
		action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusForbidden)
		action.assertUntouched(test)
	})
	test.Run("the owner's passkey approves", func(test *testing.T) {
		action := newPendingDeployLift(test, deployLiftParams)
		var out decisionActionOutcome
		decodeJSON(test, action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK), &out)
		if got := action.state(test); got != "executed" {
			test.Fatalf("state = %s, want executed", got)
		}
	})
}
