// Package governance implements the pure missing-mention judgment layer for
// Multica's workspace governance engine (see
// docs/designs/jev-workspace-governance.md and
// docs/designs/jev-missing-mention-spec.md).
//
// This package owns only the evaluator: given an already-built snapshot of
// candidates, evidence spans, and eligibility facts, it asks Jev's four
// bounded Choice questions and returns either one repair action or an
// abstention. It never queries a database, never posts a comment, and never
// selects its own candidates — building the deterministic-eligibility
// snapshot, persisting receipts, and executing an accepted action are later
// deliveries' responsibility.
//
// Evaluate takes a [Provider] rather than a concrete [jev.Client] so it can
// be exercised entirely offline with a scripted fake, with no live API key,
// network access, or agent CLI. See cmd/govreplay for a fake-HTTP replay
// harness that proves the real wire path (request shape, strict-Choice
// decoding) end to end without a live key.
package governance

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/multica-ai/multica/server/pkg/jev"
)

// MinConfidence is the offline contract default: every one of the four
// consumed Choice confidences must be at least this high, or the evaluator
// abstains. It is a conservative engineering placeholder, not a calibrated
// probability of correct execution — see the spec's confidence and
// false-positive budget section.
const MinConfidence = 0.95

// MaxCandidates and MaxSpans bound the choice sets offered to Jev. Overflow
// abstains rather than truncating: a truncated candidate or span set could
// silently drop the correct answer instead of surfacing that the input
// needs a different construction strategy.
const (
	MaxCandidates = 16
	MaxSpans      = 16
)

// Candidate is one eligible next-owner option, already filtered for
// workspace visibility, invocation permission, and runtime eligibility by
// the caller.
type Candidate struct {
	// ID is the agent or squad UUID, exactly as it would appear in a
	// mention:// link.
	ID string
	// IsSquad distinguishes a squad UID from an agent UID for the mention
	// link the executor eventually renders.
	IsSquad bool
	// DisplayName is shown to the model in the option description, not
	// used as a matching key.
	DisplayName string
	// Description states the candidate's current role and verified
	// ownership (e.g. "current accountable assignee for this issue"), not
	// just a name — the spec requires this for next_owner discrimination.
	Description string
}

// Span is one offered evidence passage: a source sentence or paragraph
// bounded by stored offsets, so a selected citation can be checked against
// live text later.
type Span struct {
	Text        string
	StartOffset int
	EndOffset   int
}

// Input is one evaluation's already-built snapshot. Candidates and Spans
// must both be non-empty and within MaxCandidates/MaxSpans; construction of
// both (dedup, visibility filtering, permission checks, offset mapping) is
// the caller's job, not this package's.
type Input struct {
	// State is the immutable snapshot sent to Jev as the request state.
	// All four questions read this same state.
	State any

	Candidates []Candidate
	Spans      []Span

	// MechanicalPreparationEligible must be independently established from
	// deterministic PR/check evidence (linked draft PR head, required
	// concluded checks, a trusted independent review/acceptance receipt,
	// no outstanding correction) before an agent_preparation decision can
	// be accepted for this source. A prose claim cannot substitute for it.
	MechanicalPreparationEligible bool

	// AccountableIndex is the offset into Candidates of the issue's current
	// accountable agent/squad assignee, or -1 if there is none. An
	// agent_preparation correction is only accepted when next_owner
	// resolves to exactly this candidate.
	AccountableIndex int

	// ExpectedModel is the pinned model the response must report having
	// used. Defaults to [jev.DefaultModel] when empty.
	ExpectedModel string
}

// Provider evaluates one Jev request. [*jev.Client] satisfies this
// directly; tests and the replay command may substitute a scripted fake.
type Provider interface {
	Evaluate(ctx context.Context, req jev.Request) (*jev.Response, error)
}

// ActionKind is the permitted correction an [Action] carries out. These are
// the only two corrections this evaluator ever selects; every other
// combination of answers abstains.
type ActionKind string

const (
	// ActionMentionOwner delivers named agent work to the resolved
	// candidate, citing the resolved evidence span.
	ActionMentionOwner ActionKind = "mention_owner"
	// ActionReturnMechanicalStep hands routine agent-owned preparation to
	// the current accountable assignee ahead of a genuine human decision.
	ActionReturnMechanicalStep ActionKind = "return_mechanical_step_to_assignee"
)

// Action is the one permitted correction the evaluator selected.
type Action struct {
	Kind      ActionKind
	Candidate Candidate
	Evidence  Span
}

// AbstainReason names why no action was produced. Every reason is
// deliberately more specific than "no", so the caller's dashboard and the
// D02V baseline comparison can distinguish "genuinely nothing to do" from
// "the model answered something the contract does not accept".
type AbstainReason string

const (
	ReasonNone                            AbstainReason = ""
	ReasonTooManyCandidates               AbstainReason = "too_many_candidates"
	ReasonTooManySpans                    AbstainReason = "too_many_spans"
	ReasonNoCandidates                    AbstainReason = "no_candidates"
	ReasonNoSpans                         AbstainReason = "no_spans"
	ReasonProviderError                   AbstainReason = "provider_error"
	ReasonInvalidResponse                 AbstainReason = "invalid_response"
	ReasonModelMismatch                   AbstainReason = "model_mismatch"
	ReasonLowConfidence                   AbstainReason = "low_confidence"
	ReasonNoFollowup                      AbstainReason = "no_followup"
	ReasonHumanDecision                   AbstainReason = "human_decision"
	ReasonUnclear                         AbstainReason = "unclear"
	ReasonContradictoryAnswers            AbstainReason = "contradictory_answers"
	ReasonNoEligibleOwner                 AbstainReason = "no_eligible_owner"
	ReasonAmbiguousOwner                  AbstainReason = "ambiguous_owner"
	ReasonNoEvidence                      AbstainReason = "no_evidence"
	ReasonAmbiguousEvidence               AbstainReason = "ambiguous_evidence"
	ReasonMechanicalPreparationIneligible AbstainReason = "mechanical_preparation_ineligible"
	ReasonOwnerMismatch                   AbstainReason = "owner_mismatch"
)

// RecordedAnswer preserves one question's raw distribution and the
// threshold it was checked against, for audit. The spec requires that a
// record "carries all raw distributions and thresholds" and forbids
// multiplying or averaging them into one overall confidence — so they are
// kept here exactly as answered, per question.
type RecordedAnswer struct {
	QuestionID    string
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
}

// Decision is the outcome of one evaluation: exactly one of Action or a
// non-empty AbstainReason is set.
type Decision struct {
	Action        *Action
	AbstainReason AbstainReason
	Answers       []RecordedAnswer
}

func abstain(reason AbstainReason, answers []RecordedAnswer) Decision {
	return Decision{AbstainReason: reason, Answers: answers}
}

// Evaluate asks the four bounded Choice questions and returns the resulting
// decision.
//
// A non-nil error is returned only alongside a [ReasonProviderError] or
// [ReasonInvalidResponse] abstention, so a caller that only cares whether an
// action was produced can check decision.Action == nil and ignore err; a
// caller that wants to log or alert on transport/validation failures can
// still inspect it.
func Evaluate(ctx context.Context, provider Provider, in Input) (Decision, error) {
	if provider == nil {
		return Decision{}, errors.New("governance: Evaluate requires a non-nil Provider")
	}
	if len(in.Candidates) > MaxCandidates {
		return abstain(ReasonTooManyCandidates, nil), nil
	}
	if len(in.Spans) > MaxSpans {
		return abstain(ReasonTooManySpans, nil), nil
	}
	if len(in.Candidates) == 0 {
		return abstain(ReasonNoCandidates, nil), nil
	}
	if len(in.Spans) == 0 {
		return abstain(ReasonNoSpans, nil), nil
	}

	spec := buildQuestionSpec(in)
	req := jev.Request{
		State:     in.State,
		Questions: spec.questions(),
	}

	resp, err := provider.Evaluate(ctx, req)
	if err != nil {
		return abstain(ReasonProviderError, nil), fmt.Errorf("governance: evaluate: %w", err)
	}

	expectedModel := in.ExpectedModel
	if expectedModel == "" {
		expectedModel = jev.DefaultModel
	}
	if resp.Model != expectedModel {
		return abstain(ReasonModelMismatch, nil), fmt.Errorf("governance: response model %q, want %q", resp.Model, expectedModel)
	}

	parsed, answers, reason, err := parseAnswers(resp, spec)
	if reason != ReasonNone {
		return abstain(reason, answers), err
	}

	for _, p := range parsed {
		if !confidenceEqualOrAbove(p.confidence, MinConfidence) {
			return abstain(ReasonLowConfidence, answers), nil
		}
	}

	return decide(in, spec, parsed, answers)
}

// parsedAnswer is one question's answer after strict validation.
type parsedAnswer struct {
	id         string
	choice     string
	confidence float64
}

// parseAnswers strictly validates all four answers before any decision
// logic runs, so a single malformed answer cannot be partially trusted.
func parseAnswers(resp *jev.Response, spec questionSpec) ([]parsedAnswer, []RecordedAnswer, AbstainReason, error) {
	ids := []string{questionHandoffKind, questionNextOwner, questionEvidence, questionCorrection}
	parsed := make([]parsedAnswer, 0, len(ids))
	recorded := make([]RecordedAnswer, 0, len(ids))

	for _, id := range ids {
		answer, err := resp.Answer(id)
		if err != nil {
			return nil, recorded, ReasonInvalidResponse, fmt.Errorf("governance: %w", err)
		}
		offered := spec.offered[id]
		choice, confidence, err := answer.AsStrictChoice(offered)
		if err != nil {
			return nil, recorded, ReasonInvalidResponse, fmt.Errorf("governance: question %q: %w", id, err)
		}
		recorded = append(recorded, RecordedAnswer{
			QuestionID:    id,
			Choice:        choice,
			Confidence:    confidence,
			Probabilities: answer.Probabilities,
		})
		parsed = append(parsed, parsedAnswer{id: id, choice: choice, confidence: confidence})
	}
	return parsed, recorded, ReasonNone, nil
}

func answerFor(parsed []parsedAnswer, id string) string {
	for _, p := range parsed {
		if p.id == id {
			return p.choice
		}
	}
	return ""
}

// decide applies the cross-answer contract: which combinations of the four
// answers are internally consistent, and which one of the two permitted
// corrections (if any) they authorize.
func decide(in Input, spec questionSpec, parsed []parsedAnswer, answers []RecordedAnswer) (Decision, error) {
	handoffKind := answerFor(parsed, questionHandoffKind)
	correction := answerFor(parsed, questionCorrection)
	nextOwner := answerFor(parsed, questionNextOwner)
	evidence := answerFor(parsed, questionEvidence)

	switch handoffKind {
	case handoffNoFollowup:
		return abstain(ReasonNoFollowup, answers), nil
	case handoffHumanDecision:
		return abstain(ReasonHumanDecision, answers), nil
	case handoffUnclear:
		return abstain(ReasonUnclear, answers), nil
	case handoffAgentWork:
		if correction != correctionMentionOwner {
			return abstain(ReasonContradictoryAnswers, answers), nil
		}
		candidateIdx, reason := resolveCandidate(spec, nextOwner)
		if reason != ReasonNone {
			return abstain(reason, answers), nil
		}
		spanIdx, reason := resolveSpan(spec, evidence)
		if reason != ReasonNone {
			return abstain(reason, answers), nil
		}
		return Decision{
			Action: &Action{
				Kind:      ActionMentionOwner,
				Candidate: in.Candidates[candidateIdx],
				Evidence:  in.Spans[spanIdx],
			},
			Answers: answers,
		}, nil
	case handoffAgentPreparation:
		if correction != correctionReturnMechanicalStep {
			return abstain(ReasonContradictoryAnswers, answers), nil
		}
		if !in.MechanicalPreparationEligible {
			return abstain(ReasonMechanicalPreparationIneligible, answers), nil
		}
		candidateIdx, reason := resolveCandidate(spec, nextOwner)
		if reason != ReasonNone {
			return abstain(reason, answers), nil
		}
		if in.AccountableIndex < 0 || candidateIdx != in.AccountableIndex {
			return abstain(ReasonOwnerMismatch, answers), nil
		}
		spanIdx, reason := resolveSpan(spec, evidence)
		if reason != ReasonNone {
			return abstain(reason, answers), nil
		}
		return Decision{
			Action: &Action{
				Kind:      ActionReturnMechanicalStep,
				Candidate: in.Candidates[candidateIdx],
				Evidence:  in.Spans[spanIdx],
			},
			Answers: answers,
		}, nil
	default:
		return abstain(ReasonContradictoryAnswers, answers), nil
	}
}

func resolveCandidate(spec questionSpec, label string) (int, AbstainReason) {
	if label == optionNone {
		return -1, ReasonNoEligibleOwner
	}
	if label == optionAmbiguous {
		return -1, ReasonAmbiguousOwner
	}
	idx, ok := spec.candidateIndex[label]
	if !ok {
		return -1, ReasonContradictoryAnswers
	}
	return idx, ReasonNone
}

func resolveSpan(spec questionSpec, label string) (int, AbstainReason) {
	if label == optionNone {
		return -1, ReasonNoEvidence
	}
	if label == optionAmbiguous {
		return -1, ReasonAmbiguousEvidence
	}
	idx, ok := spec.spanIndex[label]
	if !ok {
		return -1, ReasonContradictoryAnswers
	}
	return idx, ReasonNone
}

// confidenceEqualOrAbove is a tiny helper kept separate so the >= comparison
// reads as an intentional inclusive threshold at the call site (0.95 itself
// passes), matching the spec's "at least 0.95".
func confidenceEqualOrAbove(confidence, threshold float64) bool {
	return !math.IsNaN(confidence) && confidence >= threshold
}
