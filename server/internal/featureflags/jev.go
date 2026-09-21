package featureflags

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/jev"
)

// JevEnabled reports whether this request may spend a Jev evaluation.
func JevEnabled(ctx context.Context, flags *featureflag.Service) bool {
	return flags.IsEnabled(ctx, Jev, false)
}

// JevGate adapts the feature-flag service to [jev.Gate] so the kill switch
// lives inside the Jev client rather than at each call site. A new call site
// cannot forget to check the flag, because it never gets the chance to.
//
// A nil *featureflag.Service denies every call: failing closed is correct for
// a paid external dependency that is off by default.
type JevGate struct {
	Flags *featureflag.Service
}

// Allowed implements [jev.Gate].
func (g JevGate) Allowed(ctx context.Context) bool {
	if g.Flags == nil {
		return false
	}
	return JevEnabled(ctx, g.Flags)
}

// Compile-time proof that the adapter satisfies the client's interface.
var _ jev.Gate = JevGate{}
