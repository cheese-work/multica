package featureflags

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

func wakeupWritesService(rule *featureflag.Rule) *featureflag.Service {
	provider := featureflag.NewStaticProvider()
	if rule != nil {
		provider.LoadRules(map[string]featureflag.Rule{WakeupDefinitionWrites: *rule})
	}
	return featureflag.NewService(provider)
}

// Production must hold zero scoped wakeup definitions until the activation has
// completed, so every shape of the service without an explicit rule is closed
// until then, and open after it.
func TestWakeupDefinitionWritesFollowTheActivation(t *testing.T) {
	ctx := context.Background()
	for name, flags := range map[string]*featureflag.Service{
		"nil service":  nil,
		"nil provider": featureflag.NewService(nil),
		"unconfigured": wakeupWritesService(nil),
	} {
		if WakeupDefinitionWritesEnabled(ctx, flags, false) {
			t.Errorf("%s: writes opened before the activation completed", name)
		}
		if !WakeupDefinitionWritesEnabled(ctx, flags, true) {
			t.Errorf("%s: writes stayed closed after the activation completed", name)
		}
	}
}

// An explicit off rule is the kill switch once the activation is done, and an
// explicit on rule never opens the gate before it: the flag cannot skip the
// backfill and its verification.
func TestWakeupDefinitionWritesFlagCannotOpenBeforeTheActivation(t *testing.T) {
	ctx := context.Background()
	on, off := wakeupWritesService(&featureflag.Rule{Default: true}), wakeupWritesService(&featureflag.Rule{Default: false})
	if WakeupDefinitionWritesEnabled(ctx, on, false) {
		t.Fatal("an enabled flag opened wakeup definition writes before the activation completed")
	}
	if !WakeupDefinitionWritesEnabled(ctx, on, true) {
		t.Fatal("an enabled flag closed wakeup definition writes after the activation completed")
	}
	if WakeupDefinitionWritesEnabled(ctx, off, true) {
		t.Fatal("explicit off rule did not close wakeup definition writes after activation")
	}
}
