package middleware

import (
	"context"
	"net/http"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// evalContextFor builds the targeting context for a resolved workspace member.
//
// It deliberately takes the member row rather than the request: this is the
// part of the context that every caller of SetMemberContext can supply,
// including handlers that resolve a workspace by entity lookup instead of
// going through the workspace middleware.
//
// The attribute names (featureflags.FlagAttr*) are defined once in
// internal/featureflags/eval_attrs.go; see that file for the full set a
// Rule's AllowBy / DenyBy may name.
func evalContextFor(workspaceID string, member db.Member) featureflag.EvalContext {
	return featureflag.EvalContext{
		UserID:      util.UUIDToString(member.UserID),
		WorkspaceID: workspaceID,
		Attributes: map[string]string{
			featureflags.FlagAttrMemberID:   util.UUIDToString(member.ID),
			featureflags.FlagAttrMemberRole: member.Role,
		},
	}
}

// withAgentEvalAttributes copies ec and adds the actor attributes carried on
// the request, returning it unchanged when the request is not from a machine
// credential.
//
// This is separate from evalContextFor because agent identity lives on
// headers rather than on the member row, so only request-scoped callers can
// supply it. Copying rather than mutating keeps the input safe to reuse.
//
// X-Actor-Source and X-Agent-ID are server-stamped: middleware/auth.go strips
// any client-supplied value before re-setting it itself, so a client cannot
// choose its own flag cohort by setting these headers.
func withAgentEvalAttributes(ec featureflag.EvalContext, r *http.Request) featureflag.EvalContext {
	actorSource := r.Header.Get("X-Actor-Source")
	agentID := r.Header.Get("X-Agent-ID")
	if actorSource == "" && agentID == "" {
		return ec
	}

	attrs := make(map[string]string, len(ec.Attributes)+2)
	for k, v := range ec.Attributes {
		attrs[k] = v
	}
	if actorSource != "" {
		attrs[featureflags.FlagAttrActorSource] = actorSource
	}
	if agentID != "" {
		attrs[featureflags.FlagAttrAgentID] = agentID
	}
	ec.Attributes = attrs
	return ec
}

// withRequestEvalContext attaches the full targeting context — member plus
// actor attributes — to ctx.
func withRequestEvalContext(ctx context.Context, r *http.Request, workspaceID string, member db.Member) context.Context {
	ec := withAgentEvalAttributes(evalContextFor(workspaceID, member), r)
	return featureflag.WithEvalContext(ctx, ec)
}
