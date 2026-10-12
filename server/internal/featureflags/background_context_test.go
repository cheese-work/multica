package featureflags

import (
	"context"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// Background/scheduled work has no HTTP request to derive an EvalContext
// from. A blank workspace id must be a hard error, not a context that
// evaluates against the zero EvalContext and gets denied later for a
// confusing reason.
func TestWithBackgroundEvalContextRejectsBlankWorkspaceID(t *testing.T) {
	for name, id := range map[string]string{"empty": "", "whitespace": "   "} {
		t.Run(name, func(t *testing.T) {
			_, err := WithBackgroundEvalContext(context.Background(), id)
			if err == nil {
				t.Fatal("WithBackgroundEvalContext accepted a blank workspace id")
			}
			if !errors.Is(err, ErrBlankWorkspaceID) {
				t.Errorf("err = %v, want ErrBlankWorkspaceID", err)
			}
		})
	}
}

// A valid workspace id must produce an EvalContext carrying that workspace
// and stamped with the background actor-source attribute, using the exact
// same attribute name the HTTP middleware uses (FlagAttrActorSource) so
// targeting rules apply uniformly regardless of where a request originated.
func TestWithBackgroundEvalContextStampsWorkspaceAndActorSource(t *testing.T) {
	ctx, err := WithBackgroundEvalContext(context.Background(), testWorkspaceID)
	if err != nil {
		t.Fatalf("WithBackgroundEvalContext: %v", err)
	}

	ec := featureflag.EvalContextFrom(ctx)
	if ec.WorkspaceID != testWorkspaceID {
		t.Errorf("WorkspaceID = %q, want %q", ec.WorkspaceID, testWorkspaceID)
	}
	if got := ec.Attributes[FlagAttrActorSource]; got != ActorSourceBackground {
		t.Errorf("%s = %q, want %q", FlagAttrActorSource, got, ActorSourceBackground)
	}
}

// A background job must not be able to choose its own flag cohort: even if
// the caller's ambient context leaked in a request-derived EvalContext (an
// agent id, a different actor source), the background-derived identity
// REPLACES it rather than merging with or inheriting from it.
func TestWithBackgroundEvalContextReplacesInheritedEvalContext(t *testing.T) {
	leaked := featureflag.EvalContext{
		UserID:      "leaked-user",
		WorkspaceID: "leaked-workspace",
		Attributes: map[string]string{
			FlagAttrActorSource: "task_token",
			FlagAttrAgentID:     "leaked-agent",
		},
	}
	parent := featureflag.WithEvalContext(context.Background(), leaked)

	ctx, err := WithBackgroundEvalContext(parent, testWorkspaceID)
	if err != nil {
		t.Fatalf("WithBackgroundEvalContext: %v", err)
	}

	ec := featureflag.EvalContextFrom(ctx)
	if ec.WorkspaceID != testWorkspaceID {
		t.Errorf("WorkspaceID = %q, want the background-derived %q, not the leaked %q", ec.WorkspaceID, testWorkspaceID, leaked.WorkspaceID)
	}
	if ec.UserID != "" {
		t.Errorf("UserID = %q, want empty (background work has no user)", ec.UserID)
	}
	if got := ec.Attributes[FlagAttrActorSource]; got != ActorSourceBackground {
		t.Errorf("%s = %q, want %q, not the inherited task_token", FlagAttrActorSource, got, ActorSourceBackground)
	}
	if _, ok := ec.Attributes[FlagAttrAgentID]; ok {
		t.Error("background context inherited the leaked agent_id attribute")
	}
}

// End-to-end: the context this helper produces must be usable directly with
// the production gate, and must actually allow when the flag is on.
func TestWithBackgroundEvalContextWorksWithProductionGate(t *testing.T) {
	ctx, err := WithBackgroundEvalContext(context.Background(), testWorkspaceID)
	if err != nil {
		t.Fatalf("WithBackgroundEvalContext: %v", err)
	}

	gate := JevProductionGate{Flags: serviceWithJevRule(t, featureflag.Rule{Default: true})}
	if !gate.Allowed(ctx) {
		t.Error("production gate denied a background context with the flag on and a workspace present")
	}
}
