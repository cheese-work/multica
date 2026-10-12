package jev

import (
	"slices"
	"strings"
	"testing"
)

func TestStateBuilderKeepsEverythingUnderBudget(t *testing.T) {
	state, err := NewStateBuilder().
		Add("issue", map[string]string{"title": "Payouts failing"}, PriorityRequired).
		Add("comments", []string{"a", "b"}, PriorityUseful).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(state.Sections) != 2 {
		t.Errorf("sections = %d, want 2", len(state.Sections))
	}
	if len(state.Dropped) != 0 {
		t.Errorf("dropped %v under budget", state.Dropped)
	}
	if state.EstimatedTokens <= 0 {
		t.Error("EstimatedTokens not measured")
	}
}

// Eviction order is the whole point: optional context goes before useful
// context, and the subject of the question never goes at all.
func TestStateBuilderDropsLowestPriorityFirst(t *testing.T) {
	filler := strings.Repeat("x", 400)

	state, err := NewStateBuilder().
		WithBudget(300).
		Add("issue", filler, PriorityRequired).
		Add("siblings", filler, PriorityOptional).
		Add("evidence", filler, PriorityUseful).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, ok := state.Sections["issue"]; !ok {
		t.Error("required section was dropped")
	}
	if !slices.Contains(state.Dropped, "siblings") {
		t.Errorf("optional section survived; dropped = %v", state.Dropped)
	}
	if len(state.Dropped) > 0 && state.Dropped[0] != "siblings" {
		t.Errorf("dropped %q before the optional section", state.Dropped[0])
	}
}

// Silently answering on a state that lost its subject is worse than not
// answering, so an over-budget required section is an error.
func TestStateBuilderFailsWhenRequiredDoesNotFit(t *testing.T) {
	_, err := NewStateBuilder().
		WithBudget(10).
		Add("issue", strings.Repeat("x", 4000), PriorityRequired).
		Build()
	if err == nil {
		t.Fatal("Build accepted a required section over budget")
	}
	if !strings.Contains(err.Error(), "required state") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// The state and the longest question share one 32k budget, so a reservation
// has to come out of the state's room.
func TestStateBuilderReservesRoomForQuestions(t *testing.T) {
	sized := strings.Repeat("x", 600)

	unreserved, err := NewStateBuilder().
		WithBudget(400).
		Add("notes", sized, PriorityOptional).
		Add("issue", "small", PriorityRequired).
		Build()
	if err != nil {
		t.Fatalf("Build without reservation: %v", err)
	}

	reserved, err := NewStateBuilder().
		WithBudget(400).
		Reserve(390).
		Add("notes", sized, PriorityOptional).
		Add("issue", "small", PriorityRequired).
		Build()
	if err != nil {
		t.Fatalf("Build with reservation: %v", err)
	}

	if len(reserved.Sections) >= len(unreserved.Sections) {
		t.Errorf("reservation did not tighten the budget: %d sections vs %d",
			len(reserved.Sections), len(unreserved.Sections))
	}

	if _, err := NewStateBuilder().WithBudget(100).Reserve(100).Add("issue", "x", PriorityRequired).Build(); err == nil {
		t.Error("Build accepted a reservation consuming the whole budget")
	}
}

func TestStateBuilderAddReplacesInPlace(t *testing.T) {
	state, err := NewStateBuilder().
		Add("issue", "first", PriorityRequired).
		Add("issue", "second", PriorityRequired).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := state.Sections["issue"]; got != "second" {
		t.Errorf("issue = %v, want second", got)
	}
	if len(state.Sections) != 1 {
		t.Errorf("re-adding a key created %d sections", len(state.Sections))
	}
}

// Build must not consume the builder: the same builder rebuilt has to give
// the same answer.
func TestStateBuilderBuildIsRepeatable(t *testing.T) {
	b := NewStateBuilder().
		WithBudget(300).
		Add("issue", strings.Repeat("x", 200), PriorityRequired).
		Add("notes", strings.Repeat("y", 400), PriorityOptional)

	first, err := b.Build()
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	second, err := b.Build()
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}

	if len(first.Sections) != len(second.Sections) || first.EstimatedTokens != second.EstimatedTokens {
		t.Errorf("Build mutated the builder: %d/%d sections, %d/%d tokens",
			len(first.Sections), len(second.Sections), first.EstimatedTokens, second.EstimatedTokens)
	}
}

func TestEstimateQuestionTokensReturnsLongest(t *testing.T) {
	short := NewNoul("q?", nil)
	long := NewScore(strings.Repeat("rate this carefully ", 20), []string{"a", "b", "c"})

	longest, err := EstimateQuestionTokens(map[string]Question{"s": short, "l": long})
	if err != nil {
		t.Fatalf("EstimateQuestionTokens: %v", err)
	}

	onlyShort, err := EstimateQuestionTokens(map[string]Question{"s": short})
	if err != nil {
		t.Fatalf("EstimateQuestionTokens: %v", err)
	}
	if longest <= onlyShort {
		t.Errorf("longest = %d, not larger than the short question's %d", longest, onlyShort)
	}

	empty, err := EstimateQuestionTokens(nil)
	if err != nil || empty != 0 {
		t.Errorf("EstimateQuestionTokens(nil) = (%d, %v), want (0, nil)", empty, err)
	}
}

// The estimator is an approximation, so it must err high rather than low: an
// underestimate ships an over-budget request that fails after the latency is
// already spent.
func TestEstimateTokensIsPessimistic(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("EstimateTokens(\"\") = %d, want 0", got)
	}

	text := strings.Repeat("a", 4000)
	realistic := len(text) / 4 // roughly English at 4 chars/token
	if got := EstimateTokens(text); got <= realistic {
		t.Errorf("EstimateTokens = %d, not above the %d-token realistic count", got, realistic)
	}
}
