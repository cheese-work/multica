package featureflags

import (
	"context"
	"errors"
	"strings"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// ErrBlankWorkspaceID is returned by [WithBackgroundEvalContext] when the
// caller has no workspace identity to attach.
var ErrBlankWorkspaceID = errors.New("featureflags: blank workspace id")

// WithBackgroundEvalContext builds a flag-targeting EvalContext for
// background and scheduled work (schedulers, job handlers) that has no HTTP
// request to derive one from. Without this, a background job silently
// evaluates every flag against the zero EvalContext, which matches no
// targeting rule and buckets percent rollouts to 0 — the same gap that
// HTTP requests had before CHE-621's middleware wiring.
//
// workspaceID must be a workspace identity the job itself resolved from
// server-side substrate (for example a workspace UUID loaded from the DB
// row the job is processing) — never a value taken from unauthenticated
// input. A blank workspaceID returns [ErrBlankWorkspaceID] instead of a
// context: an evaluation with no workspace would otherwise be denied later
// by [JevProductionGate] (or fall through to Default for any other flag)
// for a reason that isn't obvious at the call site, so the empty case is
// rejected here instead.
//
// The background actor source is stamped unconditionally to
// [ActorSourceBackground] — this function takes no actor-source or agent-id
// parameter — so a caller can't choose its own flag cohort by claiming to be
// a specific agent or actor kind. Only the workspace identity, which the job
// is already trusted to have resolved, is caller-supplied.
//
// The returned context REPLACES rather than merges with any EvalContext
// already attached to parent. Background work sometimes runs off a context
// derived from request-scoped code (for example a context.Background() that
// was, at some point, cloned from a request path); if a request's
// EvalContext leaked in that way, continuing to target against it would be
// wrong — the flag decision must reflect this job's own identity, not
// whichever request happened to be in scope when the job's context was
// created.
func WithBackgroundEvalContext(parent context.Context, workspaceID string) (context.Context, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, ErrBlankWorkspaceID
	}

	ec := featureflag.EvalContext{
		WorkspaceID: workspaceID,
		Attributes: map[string]string{
			FlagAttrActorSource: ActorSourceBackground,
		},
	}
	return featureflag.WithEvalContext(parent, ec), nil
}
