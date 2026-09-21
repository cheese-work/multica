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
// A nil *featureflag.Service FAILS CLOSED — it denies every call. This is the
// deliberate inverse of [jev.Options.Gate]'s own nil behavior one layer up
// (a nil Gate there fails OPEN): failing closed is correct for a paid
// external dependency that is off by default, so this adapter must never be
// left unset in production wiring even though the client would keep working
// without it.
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

// JevProductionGate is the gate wired at server boot for real traffic
// (server/cmd/server/jev.go). Unlike [JevGate] — the plain flag adapter used
// directly by tests and other call sites that don't need workspace scoping
// — this gate also refuses to spend when the request carries no resolved
// workspace identity.
//
// That third condition is the point, not an incidental extra check: every
// jev_enabled targeting rule (an allow list, a deny list) is written against
// EvalContext.WorkspaceID. A call with an empty WorkspaceID evaluates
// against the zero EvalContext, which matches no targeting rule — so instead
// of being correctly scoped, it would silently bypass the very targeting the
// flag exists to provide. Denying it outright, rather than falling through
// to the flag's Default, is what keeps that bypass from ever spending.
type JevProductionGate struct {
	Flags *featureflag.Service
}

// Allowed implements [jev.Gate].
func (g JevProductionGate) Allowed(ctx context.Context) bool {
	if g.Flags == nil {
		return false
	}
	if featureflag.EvalContextFrom(ctx).WorkspaceID == "" {
		return false
	}
	return JevEnabled(ctx, g.Flags)
}

// Compile-time proof that the production gate satisfies the client's
// interface.
var _ jev.Gate = JevProductionGate{}
