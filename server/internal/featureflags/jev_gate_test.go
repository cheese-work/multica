package featureflags

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/jev"
)

const testWorkspaceID = "55555555-5555-5555-5555-555555555555"

func ctxWithWorkspace(workspaceID string) context.Context {
	return featureflag.WithEvalContext(context.Background(), featureflag.EvalContext{WorkspaceID: workspaceID})
}

func serviceWithJevRule(t *testing.T, rule featureflag.Rule) *featureflag.Service {
	t.Helper()
	sp := featureflag.NewStaticProvider()
	sp.LoadRules(map[string]featureflag.Rule{Jev: rule})
	return featureflag.NewService(sp)
}

// 1. Production gate denies when the flag service is nil.
func TestJevProductionGateDeniesWithoutFlagService(t *testing.T) {
	gate := JevProductionGate{}
	if gate.Allowed(ctxWithWorkspace(testWorkspaceID)) {
		t.Error("nil flag service allowed a Jev call")
	}
}

// 2. Production gate denies when jev_enabled has no rule (default off) even
// with a workspace present.
func TestJevProductionGateDeniesWithNoRuleConfigured(t *testing.T) {
	gate := JevProductionGate{Flags: featureflag.NewService(featureflag.NewStaticProvider())}
	if gate.Allowed(ctxWithWorkspace(testWorkspaceID)) {
		t.Error("Jev enabled with no rule configured")
	}
}

// 3. Production gate denies when jev_enabled is explicitly off.
func TestJevProductionGateDeniesWhenFlagExplicitlyOff(t *testing.T) {
	gate := JevProductionGate{Flags: serviceWithJevRule(t, featureflag.Rule{Default: false})}
	if gate.Allowed(ctxWithWorkspace(testWorkspaceID)) {
		t.Error("Jev enabled with flag explicitly off")
	}
}

// 4. Production gate denies when jev_enabled is on but the EvalContext has an
// empty WorkspaceID.
func TestJevProductionGateDeniesWithEmptyWorkspace(t *testing.T) {
	gate := JevProductionGate{Flags: serviceWithJevRule(t, featureflag.Rule{Default: true})}
	if gate.Allowed(ctxWithWorkspace("")) {
		t.Error("Jev enabled with an empty WorkspaceID in the EvalContext")
	}
	// A context that never had an EvalContext attached at all must behave
	// identically: EvalContextFrom returns the zero value either way.
	if gate.Allowed(context.Background()) {
		t.Error("Jev enabled with no EvalContext attached at all")
	}
}

// 5. Production gate denies when jev_enabled is on, workspace present, but a
// DenyBy rule on workspace_id targets that workspace.
func TestJevProductionGateDeniesWhenWorkspaceDenyListed(t *testing.T) {
	gate := JevProductionGate{Flags: serviceWithJevRule(t, featureflag.Rule{
		Default: true,
		Deny:    []string{testWorkspaceID},
		DenyBy:  "workspace_id",
	})}
	if gate.Allowed(ctxWithWorkspace(testWorkspaceID)) {
		t.Error("Jev enabled for a workspace on the deny list")
	}
}

// 6. Positive control: production gate allows when the flag is on and a
// workspace is present. Without this, the denial assertions above could
// pass merely because the gate always denies.
func TestJevProductionGateAllowsWhenFlagOnAndWorkspacePresent(t *testing.T) {
	gate := JevProductionGate{Flags: serviceWithJevRule(t, featureflag.Rule{Default: true})}
	if !gate.Allowed(ctxWithWorkspace(testWorkspaceID)) {
		t.Error("Jev denied despite the flag being on and a workspace present")
	}
}

// 7. Zero-outbound proof: every denial path must stop the request before it
// reaches the network. A fake server that fails the test if hit is the only
// way to prove that, since a mocked Gate can't tell us whether the real
// client's Evaluate short-circuits correctly.
func TestJevProductionGateStopsRequestBeforeNetwork(t *testing.T) {
	var deniedCalls atomic.Int32
	failIfHit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deniedCalls.Add(1)
		t.Errorf("request reached the fake TypeSafe server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{},"usage":{}}`))
	}))
	defer failIfHit.Close()

	var allowedCalls atomic.Int32
	succeeds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{},"usage":{}}`))
	}))
	defer succeeds.Close()

	newClientWithGate := func(baseURL string, gate jev.Gate) *jev.Client {
		t.Helper()
		c, err := jev.NewClient(jev.Options{APIKey: "test-key", BaseURL: baseURL, RetryCount: -1, Gate: gate})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		return c
	}

	req := jev.Request{
		State:     "state",
		Questions: map[string]jev.Question{"q": jev.NewNoul("q?", nil)},
	}

	denialCases := map[string]struct {
		gate jev.Gate
		ctx  context.Context
	}{
		"nil flag service": {
			gate: JevProductionGate{},
			ctx:  ctxWithWorkspace(testWorkspaceID),
		},
		"flag default off": {
			gate: JevProductionGate{Flags: featureflag.NewService(featureflag.NewStaticProvider())},
			ctx:  ctxWithWorkspace(testWorkspaceID),
		},
		"flag explicitly off": {
			gate: JevProductionGate{Flags: serviceWithJevRule(t, featureflag.Rule{Default: false})},
			ctx:  ctxWithWorkspace(testWorkspaceID),
		},
		"empty workspace EvalContext": {
			gate: JevProductionGate{Flags: serviceWithJevRule(t, featureflag.Rule{Default: true})},
			ctx:  ctxWithWorkspace(""),
		},
	}

	for name, tc := range denialCases {
		t.Run(name, func(t *testing.T) {
			c := newClientWithGate(failIfHit.URL, tc.gate)
			_, err := c.Evaluate(tc.ctx, req)
			if err == nil {
				t.Fatal("Evaluate succeeded but should have been denied")
			}
			if n := deniedCalls.Load(); n != 0 {
				t.Fatalf("denied call reached the fake server: %d request(s)", n)
			}
		})
	}

	// Positive control against a separate, non-failing server: proves the
	// zero-count assertions above aren't passing merely because the wiring
	// itself is broken.
	t.Run("flag on and workspace present reaches the server", func(t *testing.T) {
		c := newClientWithGate(succeeds.URL, JevProductionGate{Flags: serviceWithJevRule(t, featureflag.Rule{Default: true})})
		if _, err := c.Evaluate(ctxWithWorkspace(testWorkspaceID), req); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if n := allowedCalls.Load(); n == 0 {
			t.Fatal("allowed call never reached the fake server")
		}
	})
}
