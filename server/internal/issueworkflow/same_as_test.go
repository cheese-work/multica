package issueworkflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9114: same_as names another step of the same workflow and never leads
// back to itself.

func sameAs(status, step string) Step {
	return NormalizeHandoff(Step{StatusKey: status, Handler: Handler{Type: HandlerSameAs, Step: step}})
}

func sameAsWorkflow(steps ...Step) Definition {
	base := []Step{
		{StatusKey: "in_progress", Handler: Handler{Type: HandlerNone}},
		{StatusKey: "in_review", Handler: Handler{Type: HandlerAgent, ID: testAgentID}},
		{StatusKey: "merged", Handler: Handler{Type: HandlerNone}},
	}
	for _, s := range steps {
		for i := range base {
			if base[i].StatusKey == s.StatusKey {
				base[i] = s
			}
		}
	}
	return Definition{Steps: base}
}

func TestSameAsHandlerIsAccepted(t *testing.T) {
	step := sameAs("merged", "in_review")
	def := sameAsWorkflow(step)
	if err := ValidateHandoff(step, def); err != nil {
		t.Fatalf("same_as refused: %v", err)
	}
	if !step.HandsOff() {
		t.Fatal("a same_as step must hand off")
	}
}

func TestSameAsKeepsItsFallbackAgentAndStep(t *testing.T) {
	step := NormalizeHandoff(Step{StatusKey: "merged", Handler: Handler{Type: HandlerSameAs, Step: " in_review ", ID: testAgentID}})
	if step.Handler.Step != "in_review" || step.Handler.ID != testAgentID {
		t.Fatalf("handler = %+v, want the trimmed step and the fallback agent kept", step.Handler)
	}
	other := NormalizeHandoff(Step{StatusKey: "merged", Handler: Handler{Type: HandlerAgent, ID: testAgentID, Step: "in_review"}})
	if other.Handler.Step != "" {
		t.Fatalf("an agent handler kept step %q", other.Handler.Step)
	}
}

func TestSameAsRefusesUnknownSelfAndMissingStep(t *testing.T) {
	cases := map[string]struct {
		step Step
		want string
	}{
		"unknown":   {sameAs("merged", "nowhere"), "not a status"},
		"self":      {sameAs("merged", "merged"), "from itself"},
		"no target": {sameAs("merged", ""), "choose the step"},
	}
	for name, c := range cases {
		err := ValidateHandoff(c.step, sameAsWorkflow(c.step))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, c.want)
		}
	}
}

func TestSameAsRefusesACycle(t *testing.T) {
	a, b := sameAs("in_review", "merged"), sameAs("merged", "in_review")
	def := sameAsWorkflow(a, b)
	for _, s := range []Step{a, b} {
		if err := ValidateHandoff(s, def); err == nil || !strings.Contains(err.Error(), "leads back") {
			t.Errorf("%s: err = %v, want a cycle refusal", s.StatusKey, err)
		}
	}
	// A longer loop: in_progress -> in_review -> merged -> in_progress.
	c := sameAs("in_progress", "in_review")
	loop := sameAsWorkflow(c, sameAs("in_review", "merged"), sameAs("merged", "in_progress"))
	if err := ValidateHandoff(c, loop); err == nil {
		t.Error("a three-step loop was accepted")
	}
}

func TestSameAsAllowsAChain(t *testing.T) {
	a := sameAs("merged", "in_review")
	def := sameAsWorkflow(a, sameAs("in_review", "in_progress"))
	if err := ValidateHandoff(a, def); err != nil {
		t.Fatalf("a chain that ends at a concrete step refused: %v", err)
	}
}

func TestResolveHandlerSameAs(t *testing.T) {
	step := sameAs("merged", "in_review")
	var found, own pgtype.UUID
	if err := found.Scan(testAgentID); err != nil {
		t.Fatal(err)
	}
	if err := own.Scan("11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatal(err)
	}
	typ, id, ok := ResolveHandler(step, db.Project{}, "", pgtype.UUID{}, Humans{SameAsType: HandlerAgent, SameAsID: found})
	if !ok || typ != HandlerAgent || id != found {
		t.Fatalf("ResolveHandler = (%q, %v, %v), want the resolved agent", typ, id, ok)
	}
	if _, _, ok := ResolveHandler(step, db.Project{}, "", pgtype.UUID{}, Humans{}); ok {
		t.Fatal("a same_as step with no resolution and no own agent must resolve to nobody")
	}
	step.Handler.ID = "11111111-1111-1111-1111-111111111111"
	typ, id, ok = ResolveHandler(step, db.Project{}, "", pgtype.UUID{}, Humans{})
	if !ok || typ != HandlerAgent || id != own {
		t.Fatalf("fallback = (%q, %v, %v), want the step's own agent", typ, id, ok)
	}
}
