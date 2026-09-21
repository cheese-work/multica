package featureflags

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// A paid external dependency that is off by default must fail closed, not
// open, when the flag service is missing.
func TestJevGateDeniesWithoutFlagService(t *testing.T) {
	if (JevGate{}).Allowed(context.Background()) {
		t.Error("nil flag service allowed a Jev call")
	}
}

func TestJevGateFollowsFlag(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		sp := featureflag.NewStaticProvider()
		sp.LoadRules(map[string]featureflag.Rule{Jev: {Default: enabled}})
		gate := JevGate{Flags: featureflag.NewService(sp)}

		if got := gate.Allowed(context.Background()); got != enabled {
			t.Errorf("Allowed() = %v with flag %v", got, enabled)
		}
	}
}

// The key is off when no rule is configured at all.
func TestJevDefaultsOff(t *testing.T) {
	gate := JevGate{Flags: featureflag.NewService(featureflag.NewStaticProvider())}
	if gate.Allowed(context.Background()) {
		t.Error("Jev enabled with no rule configured")
	}
}
