package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// QuestionType is the discriminator shared by a question and its answer.
type QuestionType string

const (
	TypeNoul   QuestionType = "noul"
	TypeChoice QuestionType = "choice"
	TypeScore  QuestionType = "score"
)

// Question is one typed question in a request's question map.
//
// Instructions and Criteria are typed as any because the API accepts a
// string, object, or array for instructions, and a differently shaped
// criteria per question type. Construct questions with [NewNoul],
// [NewChoice], or [NewScore] rather than filling this in by hand — the
// constructors are what keep the criteria shape matched to the type.
type Question struct {
	Type         QuestionType `json:"type"`
	Instructions any          `json:"instructions"`
	Criteria     any          `json:"criteria,omitempty"`
}

// NoulCriteria optionally describes what a yes and a no mean.
type NoulCriteria struct {
	True  string `json:"true,omitempty"`
	False string `json:"false,omitempty"`
}

// NewNoul builds a yes/no question. Criteria are optional; pass nil to omit
// them. Because the model reads literally, phrase instructions as one
// narrow, explicit, atomic question.
func NewNoul(instructions any, criteria *NoulCriteria) Question {
	q := Question{Type: TypeNoul, Instructions: instructions}
	if criteria != nil && (criteria.True != "" || criteria.False != "") {
		q.Criteria = criteria
	}
	return q
}

// NewChoice builds a single-winner question over the given options. The
// criteria map is option name to rubric description; an empty description
// is sent as null, which the API accepts for options needing no detail.
func NewChoice(instructions any, options map[string]string) Question {
	criteria := make(map[string]*string, len(options))
	for option, description := range options {
		if description == "" {
			criteria[option] = nil
			continue
		}
		criteria[option] = &description
	}
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: criteria}
}

// NewScore builds a rubric question over ordered levels, lowest first. The
// answer is a probability-weighted value that can land between levels.
func NewScore(instructions any, levels []string) Question {
	return Question{Type: TypeScore, Instructions: instructions, Criteria: levels}
}

// Validate reports whether the question is well formed, mirroring the
// server-side rules so a malformed question fails locally instead of
// costing a 422 round trip.
func (q Question) Validate() error {
	if q.Instructions == nil {
		return errors.New("instructions are required")
	}
	if s, ok := q.Instructions.(string); ok && s == "" {
		return errors.New("instructions are required")
	}

	switch q.Type {
	case TypeNoul:
		return nil
	case TypeChoice:
		options, ok := q.Criteria.(map[string]*string)
		if !ok {
			return errors.New("choice criteria must be an option map; use NewChoice")
		}
		if len(options) < 2 {
			return fmt.Errorf("choice needs at least 2 options, got %d", len(options))
		}
		return nil
	case TypeScore:
		levels, ok := q.Criteria.([]string)
		if !ok {
			return errors.New("score criteria must be an ordered level list; use NewScore")
		}
		if len(levels) < 2 {
			return fmt.Errorf("score needs at least 2 levels, got %d", len(levels))
		}
		return nil
	default:
		return fmt.Errorf("unknown question type %q", q.Type)
	}
}

// Answer is one typed answer, returned under the same id as its question.
//
// Only the fields belonging to the answer's own type are populated. Read it
// through [Answer.AsNoul], [Answer.AsChoice], or [Answer.AsScore], which
// check the type first; reading a field directly silently yields a zero
// value when the type does not match.
type Answer struct {
	Type QuestionType `json:"type"`

	// Noul is the yes probability, 0..1. Noul answers carry no confidence.
	Noul float64 `json:"noul,omitempty"`

	// Choice is the highest-probability option.
	Choice string `json:"choice,omitempty"`

	// Score is the probability-weighted value across the levels.
	Score float64 `json:"score,omitempty"`

	// Legend maps each level index, as a string key, to its description.
	Legend map[string]string `json:"legend,omitempty"`

	// Probabilities is the full distribution, summing to 1. Keyed by option
	// for a choice and by level index for a score.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`

	// Confidence is derived from Probabilities and is present on choice and
	// score answers only.
	Confidence float64 `json:"confidence,omitempty"`

	// confidencePresent and probabilitiesPresent record whether the wire
	// payload actually carried these keys, as opposed to the field simply
	// decoding to its Go zero value. AsStrictChoice needs this distinction:
	// an omitted confidence must be rejected, not silently read as zero.
	confidencePresent    bool
	probabilitiesPresent bool
}

// UnmarshalJSON decodes an Answer normally, then separately records which
// optional keys were present in the payload so [Answer.AsStrictChoice] can
// tell an omitted field from a present-but-zero one.
func (a *Answer) UnmarshalJSON(data []byte) error {
	type alias Answer
	aux := struct{ *alias }{alias: (*alias)(a)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	var presence map[string]json.RawMessage
	if err := json.Unmarshal(data, &presence); err != nil {
		return err
	}
	a.confidencePresent = jsonKeyPresent(presence, "confidence")
	a.probabilitiesPresent = jsonKeyPresent(presence, "probabilities")
	return nil
}

func jsonKeyPresent(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	if !ok {
		return false
	}
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// AsNoul returns the yes probability.
//
// There is deliberately no confidence out-parameter: the API does not return
// one for a noul. A caller that needs a confidence gate must ask a choice
// instead of a noul, rather than inventing a confidence from the
// probability's distance to 0.5 — the vendor does not define it that way.
func (a Answer) AsNoul() (float64, error) {
	if a.Type != TypeNoul {
		return 0, fmt.Errorf("jev: answer is %q, not noul", a.Type)
	}
	return a.Noul, nil
}

// AsChoice returns the winning option and the answer's confidence.
//
// It performs no further validation: a legacy call site gets exactly the
// type and value, even when confidence was silently omitted (decoding to
// zero) or the distribution is malformed. New call sites should use
// [Answer.AsStrictChoice] instead.
func (a Answer) AsChoice() (choice string, confidence float64, err error) {
	if a.Type != TypeChoice {
		return "", 0, fmt.Errorf("jev: answer is %q, not choice", a.Type)
	}
	return a.Choice, a.Confidence, nil
}

// strictChoiceEpsilon bounds how far a probability distribution may drift
// from summing to exactly 1 before [Answer.AsStrictChoice] rejects it.
const strictChoiceEpsilon = 1e-6

// AsStrictChoice returns the winning option and confidence for a choice
// answer, validated against offered, the exact set of labels the question
// presented to the model.
//
// Unlike [Answer.AsChoice], it rejects an answer that:
//   - is not type choice;
//   - has no offered labels to validate against;
//   - omits confidence from the wire payload (a present-but-zero confidence
//     is accepted; an absent one is not — see [Answer.UnmarshalJSON]);
//   - has a confidence outside [0,1], or non-finite;
//   - omits probabilities, or does not carry exactly one entry per offered
//     label (no missing label, no extra label);
//   - has any non-finite probability or one outside [0,1];
//   - has probabilities summing further than [strictChoiceEpsilon] from 1;
//   - names a winning choice that is not one of the offered labels;
//   - names a winning choice whose probability is not the maximum of the
//     distribution (within [strictChoiceEpsilon], so ties resolve to
//     whichever label the model actually named).
//
// A caller needing a confidence gate on a plain probability should ask a
// choice, per [Answer.AsNoul]; this helper does not derive one from a Noul.
func (a Answer) AsStrictChoice(offered []string) (choice string, confidence float64, err error) {
	if a.Type != TypeChoice {
		return "", 0, fmt.Errorf("jev: answer is %q, not choice", a.Type)
	}
	if len(offered) == 0 {
		return "", 0, errors.New("jev: AsStrictChoice requires at least one offered label")
	}
	if !a.confidencePresent {
		return "", 0, errors.New("jev: choice answer omits confidence")
	}
	if math.IsNaN(a.Confidence) || math.IsInf(a.Confidence, 0) || a.Confidence < 0 || a.Confidence > 1 {
		return "", 0, fmt.Errorf("jev: choice confidence %v is not finite in [0,1]", a.Confidence)
	}
	if !a.probabilitiesPresent || len(a.Probabilities) == 0 {
		return "", 0, errors.New("jev: choice answer omits probabilities")
	}

	offeredSet := make(map[string]struct{}, len(offered))
	for _, label := range offered {
		offeredSet[label] = struct{}{}
	}
	if len(a.Probabilities) != len(offeredSet) {
		return "", 0, fmt.Errorf("jev: choice probabilities has %d label(s), want exactly the %d offered", len(a.Probabilities), len(offeredSet))
	}

	var sum float64
	maxProbability := -1.0
	for label, p := range a.Probabilities {
		if _, ok := offeredSet[label]; !ok {
			return "", 0, fmt.Errorf("jev: choice probabilities names %q, which was not offered", label)
		}
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return "", 0, fmt.Errorf("jev: choice probability for %q is %v, not finite in [0,1]", label, p)
		}
		sum += p
		if p > maxProbability {
			maxProbability = p
		}
	}
	if math.Abs(sum-1) > strictChoiceEpsilon {
		return "", 0, fmt.Errorf("jev: choice probabilities sum to %v, want 1±%v", sum, strictChoiceEpsilon)
	}

	if _, ok := offeredSet[a.Choice]; !ok {
		return "", 0, fmt.Errorf("jev: choice %q was not offered", a.Choice)
	}
	// a.Choice is a member of offeredSet, and Probabilities carries exactly
	// one entry per offered label, so this lookup always succeeds.
	winnerProbability := a.Probabilities[a.Choice]
	if math.Abs(winnerProbability-maxProbability) > strictChoiceEpsilon {
		return "", 0, fmt.Errorf("jev: choice %q (p=%v) is not the maximum-probability label (max=%v)", a.Choice, winnerProbability, maxProbability)
	}

	return a.Choice, a.Confidence, nil
}

// AsScore returns the weighted value and the answer's confidence.
func (a Answer) AsScore() (score float64, confidence float64, err error) {
	if a.Type != TypeScore {
		return 0, 0, fmt.Errorf("jev: answer is %q, not score", a.Type)
	}
	return a.Score, a.Confidence, nil
}
