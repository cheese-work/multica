package featureflags

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// Production must hold zero scoped wakeup definitions until the activation
// layer opens writes, so every unconfigured shape of the service is closed.
func TestWakeupDefinitionWritesEnabledFailsClosed(t *testing.T) {
	ctx := context.Background()
	if WakeupDefinitionWritesEnabled(ctx, nil) {
		t.Fatal("nil flag service opened wakeup definition writes")
	}
	if WakeupDefinitionWritesEnabled(ctx, featureflag.NewService(nil)) {
		t.Fatal("missing flag provider opened wakeup definition writes")
	}
	if WakeupDefinitionWritesEnabled(ctx, featureflag.NewService(featureflag.NewStaticProvider())) {
		t.Fatal("unconfigured wakeup definition writes flag was not default-off")
	}

	provider := featureflag.NewStaticProvider()
	provider.LoadRules(map[string]featureflag.Rule{WakeupDefinitionWrites: {Default: true}})
	if !WakeupDefinitionWritesEnabled(ctx, featureflag.NewService(provider)) {
		t.Fatal("explicit rule did not open wakeup definition writes")
	}
}
