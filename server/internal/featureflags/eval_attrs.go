package featureflags

// Feature-flag targeting attributes (CHE-621, extended by CHE-683).
//
// featureflag.EvalContext carries UserID and WorkspaceID as dedicated fields;
// everything else a rule can target on arrives through Attributes. These are
// the attribute names this codebase populates, and therefore the complete set
// a Rule's AllowBy / DenyBy may name today.
//
// This is the single canonical definition of these names. internal/middleware
// re-exports them (see featureflag_context.go) for its own request-scoped
// callers rather than redefining the string literals, and this package's own
// background-context helper (background_context.go) uses them directly.
//
// Every value here is derived from server-established request or job state —
// never from unauthenticated client input. See internal/middleware's
// featureflag_context.go for how the HTTP path populates these, and
// background_context.go for the background-job path.
const (
	// FlagAttrMemberID targets the workspace membership row rather than the
	// user, so a rule can enable a flag for one person in one workspace
	// without following them into another.
	FlagAttrMemberID = "member_id"

	// FlagAttrMemberRole targets the member's workspace role (owner, admin,
	// member, ...), for rollouts that should reach admins first.
	FlagAttrMemberRole = "member_role"

	// FlagAttrAgentID targets the agent a task-token request is running as.
	// Empty for human requests. On the HTTP path this is set only from the
	// server-stamped X-Agent-ID header (see internal/middleware/auth.go,
	// which strips any client-supplied value before re-setting it itself);
	// on the background path (background_context.go) it is never set at
	// all, deliberately.
	FlagAttrAgentID = "agent_id"

	// FlagAttrActorSource distinguishes the kind of caller that produced an
	// evaluation: "task_token" (agent run), "cloud_pat" (cloud node),
	// [ActorSourceBackground] (scheduled/background job), or empty (human
	// HTTP request). Always server-set, never from client input.
	FlagAttrActorSource = "actor_source"
)

// ActorSourceBackground is the FlagAttrActorSource value stamped by
// [WithBackgroundEvalContext] for scheduled/background work that has no HTTP
// request. It lets a targeting rule distinguish background evaluation from
// a user request, the same way "task_token" distinguishes an agent run.
const ActorSourceBackground = "background"
