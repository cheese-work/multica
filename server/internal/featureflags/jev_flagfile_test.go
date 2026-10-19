package featureflags

import (
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// The synthetic file in deploy/cd/fixtures is the shape the A/B deployment
// overlay mounts for MULTICA_FEATURE_FLAGS_FILE. It must enable Jev for the
// one listed workspace and no other, and never without a workspace.
func TestJevFlagFileScopesToOneWorkspace(t *testing.T) {
	rules, err := featureflag.LoadRulesFromYAMLFile("../../../deploy/cd/fixtures/jev-feature-flags.synthetic.yaml")
	if err != nil {
		t.Fatalf("load flag file: %v", err)
	}
	sp := featureflag.NewStaticProvider()
	sp.LoadRules(rules)
	gate := JevProductionGate{Flags: featureflag.NewService(sp)}

	cases := map[string]bool{
		"00000000-0000-4000-8000-000000000001": true,
		"00000000-0000-4000-8000-000000000002": false,
		"":                                     false,
	}
	for workspaceID, want := range cases {
		if got := gate.Allowed(ctxWithWorkspace(workspaceID)); got != want {
			t.Errorf("workspace %q: Allowed() = %v, want %v", workspaceID, got, want)
		}
	}
}
