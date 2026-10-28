package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const (
	passkeyRPID   = "app.example.test"
	passkeyOrigin = "https://app.example.test"
	passkeyStart  = 10
)

// passkeyHandler is a handler whose approvals are verified by a real passkey
// service for the test relying party.
func passkeyHandler(test *testing.T) *Handler {
	test.Helper()
	relyingParty, err := passkey.FromEnv(func(name string) string {
		return map[string]string{"MULTICA_WEBAUTHN_RP_ID": passkeyRPID, "MULTICA_WEBAUTHN_ORIGINS": passkeyOrigin}[name]
	})
	if err != nil || relyingParty == nil {
		test.Fatalf("relying party: %v %v", relyingParty, err)
	}
	copied := *testHandler
	copied.usePasskeys(&passkey.Service{WebAuthn: relyingParty, Queries: copied.Queries, Tx: copied.TxStarter})
	return &copied
}

// enrollPasskey gives the action's human one usable passkey.
func (action pendingAction) enrollPasskey(test *testing.T) (*passkeytest.Authenticator, string) {
	test.Helper()
	device := passkeytest.New(test)
	var id string
	dbfx.QueryRow(test, `INSERT INTO user_passkey (user_id, credential_id, public_key, sign_count, flags) VALUES ($1, $2, $3, $4, 5) RETURNING id`,
		action.humanID, device.CredentialID, device.COSEPublicKey(test), passkeyStart).Scan(&id)
	dbfx.Cleanup(test, `DELETE FROM user_passkey WHERE id = $1`, id)
	dbfx.Cleanup(test, `DELETE FROM passkey_assertion_challenge WHERE user_id = $1`, action.humanID)
	return device, id
}

type issuedChallenge struct {
	ChallengeID string `json:"challenge_id"`
	ExpiresAt   string `json:"expires_at"`
	Options     struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	} `json:"options"`
}

func (action pendingAction) challenge(test *testing.T, handler *Handler, status int) string {
	test.Helper()
	request := action.decide("challenge", action.humanID, "interactive_jwt")
	return testutil.Call(test, handler.ChallengeDecisionAction, request).Want(status).Text()
}

func (action pendingAction) issue(test *testing.T, handler *Handler) issuedChallenge {
	test.Helper()
	var issued issuedChallenge
	decodeJSON(test, action.challenge(test, handler, http.StatusOK), &issued)
	return issued
}

// assertion signs the issued challenge the way the human's authenticator does.
func (action pendingAction) assertion(test *testing.T, device *passkeytest.Authenticator, issued issuedChallenge, counter uint32) map[string]any {
	test.Helper()
	userID, err := uuid.Parse(action.humanID)
	if err != nil {
		test.Fatal(err)
	}
	signed := device.Assert(test, passkeytest.Signing{
		Challenge: issued.Options.PublicKey.Challenge, RPID: passkeyRPID, Origin: passkeyOrigin,
		Flags: passkeytest.FlagUserPresent | passkeytest.FlagUserVerified, Counter: counter, UserID: userID[:],
	})
	return map[string]any{"challenge_id": issued.ChallengeID, "credential": json.RawMessage(signed)}
}

func (action pendingAction) approveWith(test *testing.T, handler *Handler, assertion any, status int) string {
	test.Helper()
	return action.approve(test, handler, map[string]any{"payload_sha256": action.hash(test), "assertion": assertion}, status)
}

func TestChallengeDecisionActionIssuesAnAssertionChallenge(test *testing.T) {
	action := newPendingAction(test)
	action.enrollPasskey(test)
	issued := action.issue(test, passkeyHandler(test))
	if _, err := uuid.Parse(issued.ChallengeID); err != nil || issued.Options.PublicKey.Challenge == "" || issued.ExpiresAt == "" {
		test.Fatalf("incomplete challenge: %+v", issued)
	}
}

func TestChallengeDecisionActionRefusals(test *testing.T) {
	handler := passkeyHandler(test)
	member := dbfx.User(test, "Other member", "other-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": member, "role": "owner"})

	test.Run("passkeys off", func(test *testing.T) {
		action := newPendingAction(test)
		action.enrollPasskey(test)
		if body := action.challenge(test, testHandler, http.StatusForbidden); !strings.Contains(body, "approval_proof_unavailable") {
			test.Fatalf("passkeys off: %q", body)
		}
	})
	test.Run("another member and a machine actor", func(test *testing.T) {
		action := newPendingAction(test)
		action.enrollPasskey(test)
		testutil.Call(test, handler.ChallengeDecisionAction, action.decide("challenge", member, "interactive_jwt")).Want(http.StatusForbidden)
		testutil.Call(test, handler.ChallengeDecisionAction, action.decide("challenge", action.humanID, "task_token")).Want(http.StatusForbidden)
	})
	test.Run("no passkey enrolled", func(test *testing.T) {
		action := newPendingAction(test)
		if body := action.challenge(test, handler, http.StatusConflict); !strings.Contains(body, "no_passkey") {
			test.Fatalf("no passkey: %q", body)
		}
	})
	test.Run("already decided", func(test *testing.T) {
		action := newPendingAction(test)
		action.enrollPasskey(test)
		action.reject(test, handler, http.StatusOK)
		if body := action.challenge(test, handler, http.StatusConflict); !strings.Contains(body, "not_pending") {
			test.Fatalf("decided action: %q", body)
		}
	})
}

func TestApproveDecisionActionExecutesOnceWithARealAssertion(test *testing.T) {
	action := newPendingAction(test)
	device, keyID := action.enrollPasskey(test)
	handler := passkeyHandler(test)
	before, _ := action.agentEnvVersion(test)

	issued := action.issue(test, handler)
	var out decisionActionResponse
	decodeJSON(test, action.approveWith(test, handler, action.assertion(test, device, issued, passkeyStart+1), http.StatusOK), &out)
	if out.State != "executed" {
		test.Fatalf("state = %s", out.State)
	}
	if after, _ := action.agentEnvVersion(test); after != before+1 {
		test.Fatalf("env version %d, want %d", after, before+1)
	}
	var used string
	dbfx.QueryRow(test, `SELECT webauthn_credential_id::text FROM decision_action WHERE id = $1`, action.actionID).Scan(&used)
	if used != keyID {
		test.Fatalf("recorded credential %s, want %s", used, keyID)
	}
	action.approveWith(test, handler, action.assertion(test, device, issued, passkeyStart+2), http.StatusConflict)
	if after, _ := action.agentEnvVersion(test); after != before+1 {
		test.Fatal("replay changed the agent again")
	}
}

func TestApproveDecisionActionRefusesWithoutAValidAssertion(test *testing.T) {
	handler := passkeyHandler(test)
	for name, make := range map[string]func(pendingAction, *passkeytest.Authenticator, issuedChallenge) any{
		"missing": func(pendingAction, *passkeytest.Authenticator, issuedChallenge) any { return nil },
		"signed for another challenge": func(action pendingAction, device *passkeytest.Authenticator, issued issuedChallenge) any {
			issued.Options.PublicKey.Challenge = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			return action.assertion(test, device, issued, passkeyStart+1)
		},
	} {
		test.Run(name, func(test *testing.T) {
			action := newPendingAction(test)
			device, _ := action.enrollPasskey(test)
			issued := action.issue(test, handler)
			before, _ := action.agentEnvVersion(test)
			action.approveWith(test, handler, make(action, device, issued), http.StatusForbidden)
			action.assertUntouched(test)
			if after, _ := action.agentEnvVersion(test); after != before {
				test.Fatal("agent changed without a valid assertion")
			}
		})
	}
}

func TestApproveDecisionActionMarksARegressedCredentialSuspect(test *testing.T) {
	action := newPendingAction(test)
	device, keyID := action.enrollPasskey(test)
	handler := passkeyHandler(test)
	issued := action.issue(test, handler)
	action.approveWith(test, handler, action.assertion(test, device, issued, passkeyStart), http.StatusForbidden)
	var suspect bool
	dbfx.QueryRow(test, `SELECT suspect_at IS NOT NULL FROM user_passkey WHERE id = $1`, keyID).Scan(&suspect)
	if !suspect {
		test.Fatal("a regressed sign count must mark the credential suspect after the rollback")
	}
	action.assertUntouched(test)
}

func TestApproveDecisionActionRefusesAMemberWhoCannotManageTheAgentEnv(test *testing.T) {
	action := newPendingAction(test)
	action.setEnvAuthority(test, false)
	device, _ := action.enrollPasskey(test)
	handler := passkeyHandler(test)
	before, _ := action.agentEnvVersion(test)

	issued := action.issue(test, handler)
	body := action.approveWith(test, handler, action.assertion(test, device, issued, passkeyStart+1), http.StatusForbidden)
	if !strings.Contains(body, "not_permitted") {
		test.Fatalf("body %q lacks not_permitted", body)
	}
	action.assertUntouched(test)
	if after, _ := action.agentEnvVersion(test); after != before {
		test.Fatal("a member without env authority changed the agent env")
	}
}

func TestApproveDecisionActionRefusesAnApproverDemotedAfterTheChallenge(test *testing.T) {
	action := newPendingAction(test)
	action.setEnvAuthority(test, false)
	dbfx.Exec(test, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	device, _ := action.enrollPasskey(test)
	handler := passkeyHandler(test)
	before, _ := action.agentEnvVersion(test)

	issued := action.issue(test, handler)
	dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	action.approveWith(test, handler, action.assertion(test, device, issued, passkeyStart+1), http.StatusForbidden)
	action.assertUntouched(test)
	if after, _ := action.agentEnvVersion(test); after != before {
		test.Fatal("a demoted approver changed the agent env")
	}
}

func TestApproveDecisionActionExecutesForAWorkspaceAdmin(test *testing.T) {
	action := newPendingAction(test)
	action.setEnvAuthority(test, false)
	dbfx.Exec(test, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	device, _ := action.enrollPasskey(test)
	handler := passkeyHandler(test)
	before, _ := action.agentEnvVersion(test)

	issued := action.issue(test, handler)
	action.approveWith(test, handler, action.assertion(test, device, issued, passkeyStart+1), http.StatusOK)
	if after, _ := action.agentEnvVersion(test); after != before+1 {
		test.Fatalf("env version %d, want %d", after, before+1)
	}
}
