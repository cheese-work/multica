package jev

import (
	"encoding/json"
	"fmt"
	"sort"
)

// MaxStateTokens is the published ceiling for the state plus the longest
// single question in one request. The total request context is 64k, but the
// state does not get all of it.
const MaxStateTokens = 32_000

// DefaultCharsPerToken is the estimator used to budget a state without
// pulling in a tokenizer. TypeSafe publishes no tokenizer for jev-1.13, so
// this is an approximation, deliberately pessimistic: English JSON runs
// closer to 4 characters per token, and assuming fewer characters per token
// makes the estimate overshoot and the builder trim early. Budgeting is not
// the place to be optimistic — an over-budget request fails with 422 after
// the latency has already been spent.
const DefaultCharsPerToken = 3.0

// Priority orders sections for eviction. When a state does not fit, the
// lowest priority is dropped first; within one priority, the most recently
// added section goes first, so eviction is deterministic.
type Priority int

const (
	// PriorityOptional is context that improves an answer but is not what
	// the question is about — sibling issues, historical comments.
	PriorityOptional Priority = 10
	// PriorityUseful is supporting evidence a question leans on.
	PriorityUseful Priority = 20
	// PriorityRequired is the subject of the question. It is never dropped;
	// if it does not fit, [StateBuilder.Build] fails instead, because an
	// answer computed without it would be confidently about nothing.
	PriorityRequired Priority = 100
)

// State is a built, budgeted state ready to send.
type State struct {
	// Sections is the JSON object handed to the API as `state`.
	Sections map[string]any

	// EstimatedTokens is what the builder measured, by the estimator above.
	// It is an estimate, not the billed count; read the real number from
	// [Response.Usage] after the call.
	EstimatedTokens int

	// Dropped names the sections evicted to fit the budget, in eviction
	// order. A non-empty Dropped is worth logging next to the decision it
	// produced: it is the first thing to check when an answer changes for
	// no visible reason.
	Dropped []string
}

type stateSection struct {
	key      string
	value    any
	priority Priority
	order    int
}

// StateBuilder assembles a typed state that fits the token budget.
//
// Trimming is the builder's job rather than the caller's because accuracy
// degrades as a state grows: an over-long state that technically fits is
// still worse than a trimmed one. A zero StateBuilder is not usable; call
// [NewStateBuilder].
type StateBuilder struct {
	sections      []stateSection
	budget        int
	reserved      int
	charsPerToken float64
}

// NewStateBuilder returns a builder with the published budget and the
// default estimator.
func NewStateBuilder() *StateBuilder {
	return &StateBuilder{budget: MaxStateTokens, charsPerToken: DefaultCharsPerToken}
}

// WithBudget lowers (or raises, for tests) the token budget.
func (b *StateBuilder) WithBudget(tokens int) *StateBuilder {
	b.budget = tokens
	return b
}

// Reserve sets aside tokens for the longest single question in the request,
// which shares the state's budget. [Client.Evaluate] does not reserve on a
// caller's behalf — a builder used without Reserve budgets as if questions
// were free.
func (b *StateBuilder) Reserve(tokens int) *StateBuilder {
	b.reserved = tokens
	return b
}

// Add appends a named section. Re-adding a key replaces the previous value
// and keeps its original position.
func (b *StateBuilder) Add(key string, value any, priority Priority) *StateBuilder {
	for i := range b.sections {
		if b.sections[i].key == key {
			b.sections[i].value = value
			b.sections[i].priority = priority
			return b
		}
	}
	b.sections = append(b.sections, stateSection{
		key:      key,
		value:    value,
		priority: priority,
		order:    len(b.sections),
	})
	return b
}

// Build renders the state, evicting the lowest-priority sections until it
// fits. It fails when the required sections alone exceed the budget.
func (b *StateBuilder) Build() (State, error) {
	kept := make([]stateSection, len(b.sections))
	copy(kept, b.sections)

	// Eviction order: lowest priority first, then most recently added.
	evictionOrder := make([]stateSection, len(kept))
	copy(evictionOrder, kept)
	sort.SliceStable(evictionOrder, func(i, j int) bool {
		if evictionOrder[i].priority != evictionOrder[j].priority {
			return evictionOrder[i].priority < evictionOrder[j].priority
		}
		return evictionOrder[i].order > evictionOrder[j].order
	})

	available := b.budget - b.reserved
	if available <= 0 {
		return State{}, fmt.Errorf("jev: question reservation %d leaves no room in a %d token budget", b.reserved, b.budget)
	}

	var dropped []string
	for _, candidate := range evictionOrder {
		tokens, err := b.estimate(kept)
		if err != nil {
			return State{}, err
		}
		if tokens <= available {
			return b.render(kept, tokens, dropped)
		}
		if candidate.priority >= PriorityRequired {
			continue
		}
		kept = remove(kept, candidate.key)
		dropped = append(dropped, candidate.key)
	}

	tokens, err := b.estimate(kept)
	if err != nil {
		return State{}, err
	}
	if tokens > available {
		return State{}, fmt.Errorf("jev: required state is %d tokens, over the %d token budget", tokens, available)
	}
	return b.render(kept, tokens, dropped)
}

func (b *StateBuilder) render(kept []stateSection, tokens int, dropped []string) (State, error) {
	sections := make(map[string]any, len(kept))
	for _, s := range kept {
		sections[s.key] = s.value
	}
	return State{Sections: sections, EstimatedTokens: tokens, Dropped: dropped}, nil
}

func (b *StateBuilder) estimate(sections []stateSection) (int, error) {
	payload := make(map[string]any, len(sections))
	for _, s := range sections {
		payload[s.key] = s.value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("jev: encode state: %w", err)
	}
	return EstimateTokens(string(encoded)), nil
}

func remove(sections []stateSection, key string) []stateSection {
	out := sections[:0]
	for _, s := range sections {
		if s.key != key {
			out = append(out, s)
		}
	}
	return out
}

// EstimateTokens approximates the token count of a string using
// [DefaultCharsPerToken]. It is an estimate for budgeting only; the billed
// count comes back in [Response.Usage].
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	tokens := int(float64(len(s))/DefaultCharsPerToken) + 1
	return tokens
}

// EstimateQuestionTokens returns the token estimate of the largest single
// question in the map, which is the figure that shares the state's budget.
func EstimateQuestionTokens(questions map[string]Question) (int, error) {
	longest := 0
	for id, q := range questions {
		encoded, err := json.Marshal(q)
		if err != nil {
			return 0, fmt.Errorf("jev: encode question %q: %w", id, err)
		}
		if tokens := EstimateTokens(string(encoded)); tokens > longest {
			longest = tokens
		}
	}
	return longest, nil
}
