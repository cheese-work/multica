package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

const (
	guardPolicyA = `{"version":1,"plugins":[{"name":"ci-wait","match":{"tool":"gh"},"action":"deny","builtin":"ci-wait"}]}`
	guardPolicyB = `{"version":1,"plugins":[{"name":"ci-wait","match":{"tool":"gh"},"action":"pass"}]}`
)

type guardPolicyRead struct {
	Policy json.RawMessage `json:"policy"`
	SHA256 string          `json:"sha256"`
}

// guardEdit drives the owner's person-signed edit of guard_policy.
type guardEdit struct{ *gateFixture }

func newGuardEdit(test *testing.T, passkeys int) guardEdit {
	return guardEdit{newGateFixture(test, passkeys)}
}

func (g guardEdit) request(method, path, user, source string, body any) *http.Request {
	request := enrollRequest("/api/workspaces/"+g.workspace+"/guard-policy"+path, user, source, body)
	request.Method = method
	return withURLParam(request, "id", g.workspace)
}

func (g guardEdit) read() guardPolicyRead {
	g.test.Helper()
	var got guardPolicyRead
	decodeJSON(g.test, testutil.Call(g.test, g.handler.GetGuardPolicy, g.request(http.MethodGet, "", g.owner, "interactive_jwt", nil)).Want(http.StatusOK).Text(), &got)
	return got
}

func (g guardEdit) stored() string {
	g.test.Helper()
	var stored string
	dbfx.QueryRow(g.test, `SELECT COALESCE(settings->'guard_policy', 'null'::jsonb)::text FROM workspace WHERE id = $1`, g.workspace).Scan(&stored)
	return stored
}

func (g guardEdit) challenge(policy, base string, status int) (issuedChallenge, string) {
	g.test.Helper()
	body := map[string]any{"policy": json.RawMessage(policy), "base_sha256": base}
	text := testutil.Call(g.test, g.handler.ChallengeGuardPolicy, g.request(http.MethodPost, "/challenge", g.owner, "interactive_jwt", body)).Want(status).Text()
	var issued issuedChallenge
	if status == http.StatusOK {
		decodeJSON(g.test, text, &issued)
	}
	return issued, text
}

func (g guardEdit) put(policy, base string, assertion any, status int) string {
	g.test.Helper()
	body := map[string]any{"policy": json.RawMessage(policy), "base_sha256": base}
	if assertion != nil {
		body["assertion"] = assertion
	}
	return testutil.Call(g.test, g.handler.SetGuardPolicy, g.request(http.MethodPut, "", g.owner, "interactive_jwt", body)).Want(status).Text()
}

func (g guardEdit) putSigned(policy, base string, status int) string {
	g.test.Helper()
	issued, _ := g.challenge(policy, base, http.StatusOK)
	return g.put(policy, base, g.sign(0, issued), status)
}

func TestGuardPolicyEditWritesThePolicyOnceSigned(test *testing.T) {
	g := newGuardEdit(test, 1)
	before := g.read()
	g.putSigned(guardPolicyA, before.SHA256, http.StatusOK)
	after := g.read()
	if after.SHA256 == before.SHA256 {
		test.Fatal("the digest did not move after the write")
	}
	mustContain(test, string(after.Policy), `"ci-wait"`)
	mustContain(test, g.stored(), `"deny"`)
}

func TestGuardPolicyEditWithoutAnAssertionIsRefused(test *testing.T) {
	g := newGuardEdit(test, 1)
	g.put(guardPolicyA, g.read().SHA256, nil, http.StatusForbidden)
	if got := g.stored(); got != "null" {
		test.Fatalf("stored policy = %s, want it untouched", got)
	}
}

func TestGuardPolicyAssertionIsBoundToThePolicy(test *testing.T) {
	g := newGuardEdit(test, 1)
	base := g.read().SHA256
	issued, _ := g.challenge(guardPolicyA, base, http.StatusOK)
	g.put(guardPolicyB, base, g.sign(0, issued), http.StatusForbidden)
	if got := g.stored(); got != "null" {
		test.Fatalf("stored policy = %s, want it untouched", got)
	}
}

func TestGuardPolicyAssertionIsSingleUse(test *testing.T) {
	g := newGuardEdit(test, 1)
	base := g.read().SHA256
	issued, _ := g.challenge(guardPolicyA, base, http.StatusOK)
	assertion := g.sign(0, issued)
	g.put(guardPolicyA, base, assertion, http.StatusOK)
	g.put(guardPolicyA, base, assertion, http.StatusConflict)
}

func TestGuardPolicyStaleBaseIsA409WithTheCurrentVersion(test *testing.T) {
	g := newGuardEdit(test, 1)
	stale := g.read().SHA256
	g.putSigned(guardPolicyA, stale, http.StatusOK)
	current := g.read()

	// The challenge refuses a stale base, so no assertion is spent on it.
	_, text := g.challenge(guardPolicyB, stale, http.StatusConflict)
	mustContain(test, text, current.SHA256)

	// A write that carries a stale base is refused the same way and writes nothing.
	body := g.put(guardPolicyB, stale, map[string]any{"challenge_id": "00000000-0000-0000-0000-000000000000"}, http.StatusConflict)
	mustContain(test, body, current.SHA256)
	mustContain(test, body, `"deny"`) // the current policy rides in the 409 so the page can offer a re-apply
	mustContain(test, g.stored(), `"deny"`)
}

func TestGuardPolicyEditRefusesABadPolicy(test *testing.T) {
	g := newGuardEdit(test, 1)
	base := g.read().SHA256
	g.challenge(`{"version":2,"plugins":[]}`, base, http.StatusBadRequest)
	g.challenge(`null`, base, http.StatusBadRequest)
}

func TestOnlyAnInteractiveOwnerEditsTheGuardPolicy(test *testing.T) {
	g := newGuardEdit(test, 1)
	base := g.read().SHA256
	body := map[string]any{"policy": json.RawMessage(guardPolicyA), "base_sha256": base}
	for _, source := range []string{"task_token", "pat"} {
		testutil.Call(test, g.handler.ChallengeGuardPolicy, g.request(http.MethodPost, "/challenge", g.owner, source, body)).Want(http.StatusForbidden)
		testutil.Call(test, g.handler.SetGuardPolicy, g.request(http.MethodPut, "", g.owner, source, body)).Want(http.StatusForbidden)
	}
}
