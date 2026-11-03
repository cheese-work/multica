package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// gateFixture is a workspace owner with a mailer-backed passkey handler and
// any number of enrolled devices.
type gateFixture struct {
	test      *testing.T
	handler   *Handler
	mailbox   *codeMailbox
	workspace string
	owner     string
	devices   []*passkeytest.Authenticator
	keyIDs    []string
	counter   uint32
}

func newGateFixture(test *testing.T, passkeys int) *gateFixture {
	test.Helper()
	mailbox := &codeMailbox{}
	f := &gateFixture{test: test, handler: enrollHandler(test, true, mailbox), mailbox: mailbox, owner: enrollUser(test), counter: passkeyStart}
	f.workspace = dbfx.Workspace(test, "Gate workspace", "gate-"+uuid.NewString())
	dbfx.Member(test, f.workspace, f.owner, "owner")
	dbfx.Cleanup(test, `DELETE FROM passkey_assertion_challenge WHERE user_id = $1`, f.owner)
	for range passkeys {
		f.addPasskey()
	}
	return f
}

func (f *gateFixture) addPasskey() int {
	f.test.Helper()
	device := passkeytest.New(f.test)
	var id string
	dbfx.QueryRow(f.test, `INSERT INTO user_passkey (user_id, credential_id, public_key, sign_count, flags) VALUES ($1, $2, $3, $4, 5) RETURNING id`,
		f.owner, device.CredentialID, device.COSEPublicKey(f.test), passkeyStart).Scan(&id)
	f.devices, f.keyIDs = append(f.devices, device), append(f.keyIDs, id)
	return len(f.devices) - 1
}

// sign answers an issued challenge with device n; the counter only grows.
func (f *gateFixture) sign(n int, issued issuedChallenge) map[string]any {
	f.test.Helper()
	f.counter++
	userID, err := uuid.Parse(f.owner)
	if err != nil {
		f.test.Fatal(err)
	}
	signed := f.devices[n].Assert(f.test, passkeytest.Signing{
		Challenge: issued.Options.PublicKey.Challenge, RPID: passkeyRPID, Origin: passkeyOrigin,
		Flags: passkeytest.FlagUserPresent | passkeytest.FlagUserVerified, Counter: f.counter, UserID: userID[:],
	})
	return map[string]any{"challenge_id": issued.ChallengeID, "credential": json.RawMessage(signed)}
}

func (f *gateFixture) gateRequest(path, user, source string, body any) *http.Request {
	return withURLParam(enrollRequest("/api/workspaces/"+f.workspace+"/decision-gate"+path, user, source, body), "id", f.workspace)
}

func (f *gateFixture) challenge(state string, status int) string {
	f.test.Helper()
	request := f.gateRequest("/challenge", f.owner, "interactive_jwt", map[string]string{"state": state})
	return testutil.Call(f.test, f.handler.ChallengeDecisionGate, request).Want(status).Text()
}

func (f *gateFixture) issue(state string) issuedChallenge {
	f.test.Helper()
	var issued issuedChallenge
	decodeJSON(f.test, f.challenge(state, http.StatusOK), &issued)
	return issued
}

func (f *gateFixture) set(state string, assertion any, status int) string {
	f.test.Helper()
	body := map[string]any{"state": state}
	if assertion != nil {
		body["assertion"] = assertion
	}
	return testutil.Call(f.test, f.handler.SetDecisionGate, f.gateRequest("", f.owner, "interactive_jwt", body)).Want(status).Text()
}

// setSigned runs the whole ceremony with device n.
func (f *gateFixture) setSigned(n int, state string, status int) string {
	f.test.Helper()
	return f.set(state, f.sign(n, f.issue(state)), status)
}

func (f *gateFixture) gate() (state string, revision int) {
	f.test.Helper()
	dbfx.QueryRow(f.test, `SELECT decision_passkey_gate, policy_revision FROM workspace WHERE id = $1`, f.workspace).Scan(&state, &revision)
	return
}

func (f *gateFixture) forceGate(state string) {
	f.test.Helper()
	dbfx.Exec(f.test, `UPDATE workspace SET decision_passkey_gate = $2 WHERE id = $1`, f.workspace, state)
}

func (f *gateFixture) wantGate(state string, revision int) {
	f.test.Helper()
	if got, rev := f.gate(); got != state || rev != revision {
		f.test.Fatalf("gate = %s rev %d, want %s rev %d", got, rev, state, revision)
	}
}

func mustContain(test *testing.T, body, want string) {
	test.Helper()
	if !strings.Contains(body, want) {
		test.Fatalf("body = %q, want it to contain %q", body, want)
	}
}

func TestDecisionGateStartsOffAndUsersStartReady(test *testing.T) {
	f := newGateFixture(test, 0)
	f.wantGate("off", 1)
	var state string
	dbfx.QueryRow(test, `SELECT passkey_state FROM "user" WHERE id = $1`, f.owner).Scan(&state)
	if state != "ready" {
		test.Fatalf("passkey_state = %q, want ready", state)
	}
}

func TestEnablingTheGateNeedsTwoActivePasskeys(test *testing.T) {
	f := newGateFixture(test, 1)
	mustContain(test, f.challenge("block", http.StatusConflict), "two_passkeys_required")
	mustContain(test, f.set("block", f.sign(0, issuedChallenge{}), http.StatusConflict), "two_passkeys_required")
	f.wantGate("off", 1)

	f.addPasskey()
	dbfx.Exec(test, `UPDATE user_passkey SET suspect_at = now() WHERE id = $1`, f.keyIDs[1])
	mustContain(test, f.challenge("block", http.StatusConflict), "two_passkeys_required")
	f.wantGate("off", 1)
}

func TestEnablingTheGateNeedsAnAssertion(test *testing.T) {
	f := newGateFixture(test, 2)
	mustContain(test, f.set("block", nil, http.StatusForbidden), "assertion_required")
	f.wantGate("off", 1)
}

func TestEnablingAndDisablingTheGateWithAnAssertion(test *testing.T) {
	f := newGateFixture(test, 2)
	f.setSigned(0, "block", http.StatusOK)
	f.wantGate("block", 2)
	f.setSigned(1, "off", http.StatusOK)
	f.wantGate("off", 3)

	var events int
	dbfx.QueryRow(test, `SELECT count(*) FROM passkey_event WHERE user_id = $1 AND action IN ('decision_gate_enabled', 'decision_gate_disabled')`, f.owner).Scan(&events)
	if events != 2 {
		test.Fatalf("gate events = %d, want 2", events)
	}
}

func TestDisablingTheGateWithoutAnAssertionIsRefused(test *testing.T) {
	f := newGateFixture(test, 2)
	f.forceGate("block")
	mustContain(test, f.set("off", nil, http.StatusForbidden), "assertion_required")
	f.wantGate("block", 1)
}

func TestGateChangeAssertionIsBoundToTheTargetState(test *testing.T) {
	f := newGateFixture(test, 2)
	enable := f.sign(0, f.issue("block"))
	f.forceGate("block")
	mustContain(test, f.set("off", enable, http.StatusForbidden), "assertion_rejected")
	f.wantGate("block", 1)
}

func TestGateChangeAssertionIsSingleUse(test *testing.T) {
	f := newGateFixture(test, 2)
	assertion := f.sign(0, f.issue("block"))
	f.set("block", assertion, http.StatusOK)
	f.forceGate("off")
	f.set("block", assertion, http.StatusForbidden)
	f.wantGate("off", 2)
}

func TestOnlyAnInteractiveOwnerChangesTheGate(test *testing.T) {
	f := newGateFixture(test, 2)
	for _, source := range []string{"cli_jwt", "pat", "task_token", "cloud_pat", ""} {
		testutil.Call(test, f.handler.ChallengeDecisionGate, f.gateRequest("/challenge", f.owner, source, map[string]string{"state": "block"})).Want(http.StatusForbidden)
		testutil.Call(test, f.handler.SetDecisionGate, f.gateRequest("", f.owner, source, map[string]any{"state": "block", "assertion": map[string]any{}})).Want(http.StatusForbidden)
	}
	admin := enrollUser(test)
	dbfx.Member(test, f.workspace, admin, "admin")
	testutil.Call(test, f.handler.ChallengeDecisionGate, f.gateRequest("/challenge", admin, "interactive_jwt", map[string]string{"state": "block"})).Want(http.StatusForbidden)
	testutil.Call(test, f.handler.SetDecisionGate, f.gateRequest("", admin, "interactive_jwt", map[string]any{"state": "block", "assertion": map[string]any{}})).Want(http.StatusForbidden)
	f.wantGate("off", 1)
}

func TestGateChangeRefusesAnUnknownOrUnchangedState(test *testing.T) {
	f := newGateFixture(test, 2)
	f.challenge("maybe", http.StatusBadRequest)
	f.challenge("off", http.StatusConflict)
	f.forceGate("block")
	f.challenge("block", http.StatusConflict)
}

func TestRemovingPasskeysNeverDisablesTheGate(test *testing.T) {
	f := newGateFixture(test, 2)
	f.forceGate("block")
	request := withURLParam(enrollRequest("/api/passkeys/"+f.keyIDs[1]+"/revoke/challenge", f.owner, "interactive_jwt", nil), "id", f.keyIDs[1])
	var issued issuedChallenge
	decodeJSON(test, testutil.Call(test, f.handler.ChallengePasskeyRevocation, request).Want(http.StatusOK).Text(), &issued)
	revoke := withURLParam(enrollRequest("/api/passkeys/"+f.keyIDs[1]+"/revoke", f.owner, "interactive_jwt", map[string]any{"assertion": f.sign(0, issued)}), "id", f.keyIDs[1])
	testutil.Call(test, f.handler.RevokePasskey, revoke).Want(http.StatusOK)
	f.wantGate("block", 1)
}

type gatedEnrollment struct {
	startedEnrollment
	Assertion *issuedChallenge `json:"assertion"`
}

func (f *gateFixture) startEnroll() gatedEnrollment {
	f.test.Helper()
	var started gatedEnrollment
	decodeJSON(f.test, testutil.Call(f.test, f.handler.StartPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/start", f.owner, "interactive_jwt", nil)).Want(http.StatusOK).Text(), &started)
	return started
}

func (f *gateFixture) finishEnroll(started gatedEnrollment, assertion any, status int) string {
	f.test.Helper()
	body := finishBody(f.test, started.startedEnrollment, f.mailbox.code(f.test))
	if assertion != nil {
		body["assertion"] = assertion
	}
	return testutil.Call(f.test, f.handler.FinishPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/finish", f.owner, "interactive_jwt", body)).Want(status).Text()
}

func TestEnrollingWhileTheGateBlocksNeedsAnExistingPasskeyAssertion(test *testing.T) {
	f := newGateFixture(test, 1)
	f.forceGate("block")
	started := f.startEnroll()
	if started.Assertion == nil || started.Assertion.ChallengeID == "" {
		test.Fatalf("start returned no assertion challenge: %+v", started)
	}
	mustContain(test, f.finishEnroll(started, nil, http.StatusForbidden), "assertion_required")
	if credentialCount(test, f.owner) != 1 {
		test.Fatal("a passkey was enrolled without an assertion")
	}
	f.finishEnroll(started, f.sign(0, *started.Assertion), http.StatusOK)
	if credentialCount(test, f.owner) != 2 {
		test.Fatalf("credentials = %d, want 2", credentialCount(test, f.owner))
	}
}

func TestEnrollmentAssertionFromAnotherPurposeIsRejected(test *testing.T) {
	f := newGateFixture(test, 2)
	f.forceGate("block")
	started := f.startEnroll()
	wrongPurpose := f.sign(0, f.issue("off"))
	mustContain(test, f.finishEnroll(started, wrongPurpose, http.StatusForbidden), "assertion_rejected")
	if credentialCount(test, f.owner) != 2 {
		test.Fatal("a passkey was enrolled on a wrong-purpose assertion")
	}
}

func TestEnrollingNeedsNoAssertionWhileTheGateIsOff(test *testing.T) {
	f := newGateFixture(test, 1)
	started := f.startEnroll()
	if started.Assertion != nil {
		test.Fatalf("assertion issued with the gate off: %+v", started.Assertion)
	}
	f.finishEnroll(started, nil, http.StatusOK)
	if credentialCount(test, f.owner) != 2 {
		test.Fatalf("credentials = %d, want 2", credentialCount(test, f.owner))
	}
}

func TestFirstPasskeyNeedsNoAssertionWhileTheGateBlocks(test *testing.T) {
	f := newGateFixture(test, 0)
	f.forceGate("block")
	started := f.startEnroll()
	if started.Assertion != nil {
		test.Fatalf("assertion issued to a user with no passkey: %+v", started.Assertion)
	}
	f.finishEnroll(started, nil, http.StatusOK)
}

func (f *gateFixture) revokeByCode(target string, startStatus int) string {
	f.test.Helper()
	request := withURLParam(enrollRequest("/api/passkeys/"+target+"/revoke/code", f.owner, "interactive_jwt", nil), "id", target)
	return testutil.Call(f.test, f.handler.StartPasskeyRevocationByCode, request).Want(startStatus).Text()
}

func (f *gateFixture) confirmRevokeByCode(target, ceremony string, status int) string {
	f.test.Helper()
	body := map[string]any{"ceremony_id": ceremony, "code": f.mailbox.code(f.test)}
	request := withURLParam(enrollRequest("/api/passkeys/"+target+"/revoke/confirm", f.owner, "interactive_jwt", body), "id", target)
	return testutil.Call(f.test, f.handler.ConfirmPasskeyRevocationByCode, request).Want(status).Text()
}

func (f *gateFixture) revoked(id string) bool {
	f.test.Helper()
	var revoked bool
	dbfx.QueryRow(f.test, `SELECT revoked_at IS NOT NULL FROM user_passkey WHERE id = $1`, id).Scan(&revoked)
	return revoked
}

func TestEmailCodeRevocationIsRefusedBelowTwoPasskeysWhileTheGateBlocks(test *testing.T) {
	f := newGateFixture(test, 2)
	f.forceGate("block")
	mustContain(test, f.revokeByCode(f.keyIDs[1], http.StatusConflict), "passkey_floor")
	if f.revoked(f.keyIDs[1]) {
		test.Fatal("revoked below the floor")
	}
}

func TestEmailCodeRevocationConfirmRechecksTheFloor(test *testing.T) {
	f := newGateFixture(test, 2)
	var started struct {
		CeremonyID string `json:"ceremony_id"`
	}
	decodeJSON(test, f.revokeByCode(f.keyIDs[1], http.StatusOK), &started)
	f.forceGate("block")
	mustContain(test, f.confirmRevokeByCode(f.keyIDs[1], started.CeremonyID, http.StatusConflict), "passkey_floor")
	if f.revoked(f.keyIDs[1]) {
		test.Fatal("revoked below the floor")
	}
}

func TestEmailCodeRevocationAboveTheFloorStillWorksWhileTheGateBlocks(test *testing.T) {
	f := newGateFixture(test, 3)
	f.forceGate("block")
	var started struct {
		CeremonyID string `json:"ceremony_id"`
	}
	decodeJSON(test, f.revokeByCode(f.keyIDs[2], http.StatusOK), &started)
	f.confirmRevokeByCode(f.keyIDs[2], started.CeremonyID, http.StatusOK)
	if !f.revoked(f.keyIDs[2]) {
		test.Fatal("the passkey was not revoked")
	}
}

func TestEmailCodeRevocationBelowTwoStillWorksWhileTheGateIsOff(test *testing.T) {
	f := newGateFixture(test, 2)
	var started struct {
		CeremonyID string `json:"ceremony_id"`
	}
	decodeJSON(test, f.revokeByCode(f.keyIDs[1], http.StatusOK), &started)
	f.confirmRevokeByCode(f.keyIDs[1], started.CeremonyID, http.StatusOK)
}

// holdRowLock keeps a transaction open on the rows the query locks, the way a
// concurrent request would between its read and its commit. The lock drops at
// the end of the test, or earlier with the returned release.
func holdRowLock(test *testing.T, query string, args ...any) (release func()) {
	test.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		tx.Rollback(ctx)
		test.Fatal(err)
	}
	release = func() { tx.Rollback(ctx) }
	test.Cleanup(release)
	return release
}

func (f *gateFixture) suspect(n int) {
	f.test.Helper()
	dbfx.Exec(f.test, `UPDATE user_passkey SET suspect_at = now() WHERE id = $1`, f.keyIDs[n])
}

func TestEnrollingWhileTheGateBlocksFailsClosedWhenOnlySuspectPasskeysRemain(test *testing.T) {
	f := newGateFixture(test, 1)
	f.forceGate("block")
	f.suspect(0)
	body := testutil.Call(test, f.handler.StartPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/start", f.owner, "interactive_jwt", nil)).Want(http.StatusConflict).Text()
	mustContain(test, body, "no_usable_passkey")
	if credentialCount(test, f.owner) != 1 {
		test.Fatal("a passkey was enrolled for a user whose keys cannot sign")
	}
}

func TestEnrollmentFinishFailsClosedWhenPasskeysTurnSuspectAfterStart(test *testing.T) {
	f := newGateFixture(test, 1)
	started := f.startEnroll()
	f.forceGate("block")
	f.suspect(0)
	mustContain(test, f.finishEnroll(started, nil, http.StatusForbidden), "assertion_required")
	if credentialCount(test, f.owner) != 1 {
		test.Fatal("a passkey was enrolled without an assertion")
	}
}

func TestEnrollmentFinishWaitsOutAGateChangeInFlight(test *testing.T) {
	f := newGateFixture(test, 2)
	started := f.startEnroll()
	holdRowLock(test, `SELECT id FROM workspace WHERE id = $1 FOR UPDATE`, f.workspace)
	mustContain(test, f.finishEnroll(started, nil, http.StatusConflict), "busy_retry")
	if credentialCount(test, f.owner) != 2 {
		test.Fatal("a passkey was enrolled while the gate was changing")
	}
}

func TestEmailCodeRevocationConfirmWaitsOutAGateChangeInFlight(test *testing.T) {
	f := newGateFixture(test, 3)
	var started struct {
		CeremonyID string `json:"ceremony_id"`
	}
	decodeJSON(test, f.revokeByCode(f.keyIDs[2], http.StatusOK), &started)
	holdRowLock(test, `SELECT id FROM workspace WHERE id = $1 FOR UPDATE`, f.workspace)
	mustContain(test, f.confirmRevokeByCode(f.keyIDs[2], started.CeremonyID, http.StatusConflict), "busy_retry")
	if f.revoked(f.keyIDs[2]) {
		test.Fatal("a passkey was revoked while the gate was changing")
	}
}

// A membership change in flight (the joining user's row is held until its
// transaction commits) could add a blocking workspace the check never saw.
func TestEnrollmentFinishWaitsOutAMembershipChangeInFlight(test *testing.T) {
	f := newGateFixture(test, 2)
	started := f.startEnroll()
	holdRowLock(test, `SELECT id FROM "user" WHERE id = $1 FOR NO KEY UPDATE`, f.owner)
	mustContain(test, f.finishEnroll(started, nil, http.StatusConflict), "busy_retry")
	if credentialCount(test, f.owner) != 2 {
		test.Fatal("a passkey was enrolled while a membership was changing")
	}
}

func TestEmailCodeRevocationConfirmWaitsOutAMembershipChangeInFlight(test *testing.T) {
	f := newGateFixture(test, 3)
	var started struct {
		CeremonyID string `json:"ceremony_id"`
	}
	decodeJSON(test, f.revokeByCode(f.keyIDs[2], http.StatusOK), &started)
	holdRowLock(test, `SELECT id FROM "user" WHERE id = $1 FOR NO KEY UPDATE`, f.owner)
	mustContain(test, f.confirmRevokeByCode(f.keyIDs[2], started.CeremonyID, http.StatusConflict), "busy_retry")
	if f.revoked(f.keyIDs[2]) {
		test.Fatal("a passkey was revoked while a membership was changing")
	}
}

// The other side of the same lock: a join waits for a gate check in flight, so
// the check's membership read stays true until it commits.
func TestJoiningAWorkspaceWaitsOutAGateCheckInFlight(test *testing.T) {
	email := "gate-join-" + uuid.NewString() + "@multica.ai"
	userID := dbfx.User(test, "Gate Join Invitee", email)
	invitationID := dbfx.Insert(test, "workspace_invitation", testutil.Cols{
		"workspace_id":    testWorkspaceID,
		"inviter_id":      testUserID,
		"invitee_email":   email,
		"invitee_user_id": userID,
		"role":            "member",
		"status":          "pending",
		"expires_at":      testutil.Raw("now() + interval '1 day'"),
	})
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, userID)
	})
	release := holdRowLock(test, `SELECT id FROM "user" WHERE id = $1 FOR SHARE`, userID)

	joined := make(chan struct{})
	go func() {
		defer close(joined)
		req := newRequest("POST", "/api/invitations/"+invitationID+"/accept", nil)
		req.Header.Set("X-User-ID", userID)
		req = withURLParam(req, "id", invitationID)
		testutil.Call(test, testHandler.AcceptInvitation, req).Want(http.StatusOK)
	}()
	select {
	case <-joined:
		test.Fatal("the join committed while a gate check held the user")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		test.Fatal("the join never finished after the gate check ended")
	}
}

// Creating a workspace adds the creator's owner membership, so it waits for a
// gate check in flight too; otherwise the new workspace could be switched to
// block before that check commits.
func TestCreatingAWorkspaceWaitsOutAGateCheckInFlight(test *testing.T) {
	userID := dbfx.User(test, "Gate Creator", "gate-create-"+uuid.NewString()+"@multica.ai")
	slug := "gate-create-" + uuid.NewString()
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM member WHERE user_id = $1`, userID)
		dbfx.Exec(test, `DELETE FROM workspace WHERE slug = $1`, slug)
	})
	release := holdRowLock(test, `SELECT id FROM "user" WHERE id = $1 FOR SHARE`, userID)

	created := make(chan struct{})
	go func() {
		defer close(created)
		req := newRequest("POST", "/api/workspaces", map[string]string{"name": "Gate Create", "slug": slug})
		req.Header.Set("X-User-ID", userID)
		testutil.Call(test, testHandler.CreateWorkspace, req).Want(http.StatusCreated)
	}()
	select {
	case <-created:
		test.Fatal("the workspace was created while a gate check held the user")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case <-created:
	case <-time.After(10 * time.Second):
		test.Fatal("the workspace was never created after the gate check ended")
	}
}

func TestEnablingTheGateWaitsOutAPasskeyRemovalInFlight(test *testing.T) {
	f := newGateFixture(test, 2)
	assertion := f.sign(0, f.issue("block"))
	holdRowLock(test, `SELECT id FROM user_passkey WHERE id = $1 FOR UPDATE`, f.keyIDs[1])
	mustContain(test, f.set("block", assertion, http.StatusConflict), "busy_retry")
	f.wantGate("off", 1)
}
