package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

func uuidFor(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("scan uuid %q: %v", s, err)
	}
	return u
}

const (
	testUserID   = "11111111-1111-1111-1111-111111111111"
	testMemberID = "22222222-2222-2222-2222-222222222222"
	testWsID     = "33333333-3333-3333-3333-333333333333"
	testAgentID  = "44444444-4444-4444-4444-444444444444"
)

func testMember(t *testing.T) db.Member {
	t.Helper()
	return db.Member{
		ID:          uuidFor(t, testMemberID),
		WorkspaceID: uuidFor(t, testWsID),
		UserID:      uuidFor(t, testUserID),
		Role:        "admin",
	}
}

// SetMemberContext is the funnel every resolved-member path goes through, so
// the targeting context must be installed there and not only in the HTTP
// middleware.
func TestSetMemberContextInstallsEvalContext(t *testing.T) {
	ctx := SetMemberContext(context.Background(), testWsID, testMember(t))

	ec := featureflag.EvalContextFrom(ctx)
	if ec.UserID != testUserID {
		t.Errorf("UserID = %q, want %q", ec.UserID, testUserID)
	}
	if ec.WorkspaceID != testWsID {
		t.Errorf("WorkspaceID = %q, want %q", ec.WorkspaceID, testWsID)
	}
	if got := ec.Attributes[FlagAttrMemberID]; got != testMemberID {
		t.Errorf("%s = %q, want %q", FlagAttrMemberID, got, testMemberID)
	}
	if got := ec.Attributes[FlagAttrMemberRole]; got != "admin" {
		t.Errorf("%s = %q, want admin", FlagAttrMemberRole, got)
	}
}

// Lookup is what a Rule's AllowBy / DenyBy calls, so the well-known names and
// the attribute names must both resolve through it.
func TestEvalContextLookupResolvesTargetingNames(t *testing.T) {
	ctx := SetMemberContext(context.Background(), testWsID, testMember(t))
	ec := featureflag.EvalContextFrom(ctx)

	for _, tc := range []struct{ name, want string }{
		{"user_id", testUserID},
		{"workspace_id", testWsID},
		{FlagAttrMemberID, testMemberID},
		{FlagAttrMemberRole, "admin"},
	} {
		got, ok := ec.Lookup(tc.name)
		if !ok || got != tc.want {
			t.Errorf("Lookup(%q) = (%q, %v), want (%q, true)", tc.name, got, ok, tc.want)
		}
	}
}

// A human request carries no actor headers and must not gain agent attributes.
func TestWithAgentEvalAttributesHumanRequestUnchanged(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	base := evalContextFor(testWsID, testMember(t))

	ec := withAgentEvalAttributes(base, r)

	if _, ok := ec.Attributes[FlagAttrAgentID]; ok {
		t.Error("human request gained an agent_id attribute")
	}
	if _, ok := ec.Attributes[FlagAttrActorSource]; ok {
		t.Error("human request gained an actor_source attribute")
	}
}

func TestWithAgentEvalAttributesTaskToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Actor-Source", "task_token")
	r.Header.Set("X-Agent-ID", testAgentID)

	ec := withAgentEvalAttributes(evalContextFor(testWsID, testMember(t)), r)

	if got := ec.Attributes[FlagAttrAgentID]; got != testAgentID {
		t.Errorf("%s = %q, want %q", FlagAttrAgentID, got, testAgentID)
	}
	if got := ec.Attributes[FlagAttrActorSource]; got != "task_token" {
		t.Errorf("%s = %q, want task_token", FlagAttrActorSource, got)
	}
	// Member attributes must survive the copy.
	if got := ec.Attributes[FlagAttrMemberRole]; got != "admin" {
		t.Errorf("member role lost during enrichment: %q", got)
	}
}

// Enrichment must not write through to the caller's map.
func TestWithAgentEvalAttributesDoesNotMutateInput(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Agent-ID", testAgentID)

	base := evalContextFor(testWsID, testMember(t))
	_ = withAgentEvalAttributes(base, r)

	if _, ok := base.Attributes[FlagAttrAgentID]; ok {
		t.Error("withAgentEvalAttributes mutated the input EvalContext")
	}
}

// The end-to-end point of CHE-621: a rule can now target one workspace.
// Against the zero EvalContext the AllowBy lookup always missed, so a
// workspace allow list could never match and flags shipped as global
// switches instead.
//
// Allow lists, not percent rollouts, are what proves the wiring: inPercent
// short-circuits percent >= 100 to true without reading the identifier, so a
// 100% rollout turns on even for an unwired request and would pass this test
// for the wrong reason.
func TestWorkspaceAllowListCanTargetWorkspace(t *testing.T) {
	svc := serviceWithRule(t, featureflag.Rule{
		Default: false,
		Allow:   []string{testWsID},
		AllowBy: "workspace_id",
	})

	if svc.IsEnabled(context.Background(), testFlagKey, false) {
		t.Fatal("flag enabled without an EvalContext; test cannot prove anything")
	}

	ctx := SetMemberContext(context.Background(), testWsID, testMember(t))
	if !svc.IsEnabled(ctx, testFlagKey, false) {
		t.Error("workspace allow list did not match a request with a populated EvalContext")
	}
}

// Multi-level rules (workspace / agent / squad / project) need DenyBy to
// resolve an arbitrary attribute name, not just the two well-known ones. A
// per-agent kill switch is the case that has to work.
func TestAgentDenyListCanTargetAgent(t *testing.T) {
	svc := serviceWithRule(t, featureflag.Rule{
		Default: true,
		Deny:    []string{testAgentID},
		DenyBy:  FlagAttrAgentID,
	})

	human := httptest.NewRequest(http.MethodGet, "/", nil)
	humanCtx := withRequestEvalContext(context.Background(), human, testWsID, testMember(t))
	if !svc.IsEnabled(humanCtx, testFlagKey, false) {
		t.Error("human request was denied by an agent-scoped kill switch")
	}

	agent := httptest.NewRequest(http.MethodGet, "/", nil)
	agent.Header.Set("X-Actor-Source", "task_token")
	agent.Header.Set("X-Agent-ID", testAgentID)
	agentCtx := withRequestEvalContext(context.Background(), agent, testWsID, testMember(t))
	if svc.IsEnabled(agentCtx, testFlagKey, false) {
		t.Error("agent kill switch did not reach the named agent's request")
	}
}

const testFlagKey = "jev_test_flag"

func serviceWithRule(t *testing.T, rule featureflag.Rule) *featureflag.Service {
	t.Helper()
	sp := featureflag.NewStaticProvider()
	sp.LoadRules(map[string]featureflag.Rule{testFlagKey: rule})
	return featureflag.NewService(sp)
}
