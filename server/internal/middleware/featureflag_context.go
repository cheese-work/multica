package middleware

import (
	"context"
	"net/http"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// Feature-flag targeting attributes (CHE-621).
//
// featureflag.EvalContext carries UserID and WorkspaceID as dedicated fields;
// everything else a rule can target on arrives through Attributes. These are
// the attribute names this codebase populates, and therefore the complete set
// a Rule's AllowBy / DenyBy may name today.
//
// Every value here is derived from server-established request state: the
// member row the workspace middleware already loaded, or a header the Auth
// middleware strips from client input before re-setting it itself
// (see middleware/auth.go and handler/actor_guards.go). A client cannot
// choose which flag cohort it lands in by setting a header.
const (
	// FlagAttrMemberID targets the workspace membership row rather than the
	// user, so a rule can enable a flag for one person in one workspace
	// without following them into another.
	FlagAttrMemberID = "member_id"

	// FlagAttrMemberRole targets the member's workspace role (owner, admin,
	// member, ...), for rollouts that should reach admins first.
	FlagAttrMemberRole = "member_role"

	// FlagAttrAgentID targets the agent a task-token request is running as.
	// Empty for human requests. Set only from the server-stamped X-Agent-ID
	// header, which middleware/auth.go deletes from client input (MUL-3428)
	// and re-sets only on the mat_ task-token branch.
	FlagAttrAgentID = "agent_id"

	// FlagAttrActorSource distinguishes machine credentials from humans:
	// "task_token" (agent run), "cloud_pat" (cloud node), or empty (human).
	// Server-set only; see handler/actor_guards.go for why this header is
	// the single source of truth for "is this a machine credential?".
	FlagAttrActorSource = "actor_source"
)

// evalContextFor builds the targeting context for a resolved workspace member.
//
// It deliberately takes the member row rather than the request: this is the
// part of the context that every caller of SetMemberContext can supply,
// including handlers that resolve a workspace by entity lookup instead of
// going through the workspace middleware.
func evalContextFor(workspaceID string, member db.Member) featureflag.EvalContext {
	return featureflag.EvalContext{
		UserID:      util.UUIDToString(member.UserID),
		WorkspaceID: workspaceID,
		Attributes: map[string]string{
			FlagAttrMemberID:   util.UUIDToString(member.ID),
			FlagAttrMemberRole: member.Role,
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
		attrs[FlagAttrActorSource] = actorSource
	}
	if agentID != "" {
		attrs[FlagAttrAgentID] = agentID
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
