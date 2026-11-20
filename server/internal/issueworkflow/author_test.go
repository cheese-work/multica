package issueworkflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-7677: the author handler hands an issue to the agent its owner resolved
// from the sticky row or capability_routing; the step stores no id.

func TestAuthorHandlerIsAcceptedWithoutAnID(t *testing.T) {
	def := devDefinition()
	step := NormalizeHandoff(Step{StatusKey: "implement", Handler: Handler{Type: HandlerAuthor, ID: testAgentID}})
	if step.Handler.ID != "" {
		t.Fatalf("author handoff kept id %q, want none", step.Handler.ID)
	}
	if err := ValidateHandoff(step, def); err != nil {
		t.Fatalf("author handler refused: %v", err)
	}
	if !step.HandsOff() {
		t.Fatal("an author step must hand off")
	}
}

func TestResolveHandlerAuthor(t *testing.T) {
	step := Step{StatusKey: "in_progress", Handler: Handler{Type: HandlerAuthor}}
	var author pgtype.UUID
	if err := author.Scan(testAgentID); err != nil {
		t.Fatal(err)
	}
	typ, id, ok := ResolveHandler(step, db.Project{}, "", pgtype.UUID{}, Humans{AuthorType: HandlerAgent, AuthorID: author})
	if !ok || typ != HandlerAgent || id != author {
		t.Fatalf("ResolveHandler = (%q, %v, %v), want the author agent", typ, id, ok)
	}
	if _, _, ok := ResolveHandler(step, db.Project{}, "", pgtype.UUID{}, Humans{}); ok {
		t.Fatal("an author step with no author must resolve to nobody")
	}
}
