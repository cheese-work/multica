package featureflags

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// CHE-4257: the CI token broker stays off unless the flag file turns it on.
func TestCITokenBrokerDefaultsOff(t *testing.T) {
	ctx := context.Background()
	for name, flags := range map[string]*featureflag.Service{
		"nil service":  nil,
		"nil provider": featureflag.NewService(nil),
		"no rule":      featureflag.NewService(featureflag.NewStaticProvider()),
	} {
		if CITokenBrokerEnabled(ctx, flags) {
			t.Errorf("%s: ci_token_broker is on without a rule", name)
		}
	}
}

func TestCITokenBrokerReadsTheFlagFile(t *testing.T) {
	ctx := context.Background()
	for body, want := range map[string]bool{
		"ci_token_broker:\n  default: true\n":  true,
		"ci_token_broker:\n  default: false\n": false,
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
		if got := CITokenBrokerEnabled(ctx, flags); got != want {
			t.Errorf("flag file %q: ci_token_broker = %v, want %v", body, got, want)
		}
	}
}

func TestCITokenBrokerIsNotPublishedToFrontends(t *testing.T) {
	flags := featureflag.NewService(featureflag.NewStaticProvider())
	if _, ok := EvaluateFrontendPublicFlags(context.Background(), flags)[CITokenBroker]; ok {
		t.Fatal("ci_token_broker must not be a frontend public flag")
	}
}
