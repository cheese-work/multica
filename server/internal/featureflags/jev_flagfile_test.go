package featureflags

import (
	"path/filepath"
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

// The corpus under deploy/cd/fixtures/flags backs the controller's pre-drain
// flag-file check (deploy/cd/flagfile-check.mjs). The backend loader is the
// authority: valid/ and strict/ must load, invalid/ must be rejected. strict/
// holds files the loader accepts but the controller deliberately refuses
// (it only admits a documented subset).
func TestFlagFileCorpusMatchesLoader(t *testing.T) {
	for dir, wantLoad := range map[string]bool{"valid": true, "strict": true, "invalid": false} {
		files, err := filepath.Glob("../../../deploy/cd/fixtures/flags/" + dir + "/*.yaml")
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: no corpus files (err=%v)", dir, err)
		}
		for _, file := range files {
			_, err := featureflag.LoadRulesFromYAMLFile(file)
			if (err == nil) != wantLoad {
				t.Errorf("%s: load error = %v, want loadable = %v", file, err, wantLoad)
			}
		}
	}
}
