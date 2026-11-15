package featureflags

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// CHE-4273: the CI failure watcher stays off unless MULTICA_FEATURE_FLAGS_FILE
// turns ci_watcher on. Every shape of the service without that rule is closed.
func TestCIWatcherDefaultsOff(t *testing.T) {
	ctx := context.Background()
	for name, flags := range map[string]*featureflag.Service{
		"nil service":  nil,
		"nil provider": featureflag.NewService(nil),
		"no rule":      featureflag.NewService(featureflag.NewStaticProvider()),
	} {
		if CIWatcherEnabled(ctx, flags) {
			t.Errorf("%s: ci_watcher is on without a rule", name)
		}
	}
}

func TestCIWatcherReadsTheFlagFile(t *testing.T) {
	ctx := context.Background()
	for body, want := range map[string]bool{
		"ci_watcher:\n  default: true\n":  true,
		"ci_watcher:\n  default: false\n": false,
		"other_flag:\n  default: true\n":  false,
	} {
		file := filepath.Join(t.TempDir(), "flags.yaml")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(featureflag.EnvFlagFile, file)
		flags, err := featureflag.NewServiceFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if got := CIWatcherEnabled(ctx, flags); got != want {
			t.Errorf("flag file %q: ci_watcher = %v, want %v", body, got, want)
		}
	}
}

// The watcher is server-side only; the web client never sees the flag.
func TestCIWatcherIsNotPublishedToFrontends(t *testing.T) {
	flags := featureflag.NewService(featureflag.NewStaticProvider())
	if _, ok := EvaluateFrontendPublicFlags(context.Background(), flags)[CIWatcher]; ok {
		t.Fatal("ci_watcher must not be a frontend public flag")
	}
}
