package featureflags

import (
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

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
