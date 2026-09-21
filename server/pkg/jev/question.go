package jev

import (
	"errors"
	"fmt"
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
func (a Answer) AsChoice() (choice string, confidence float64, err error) {
	if a.Type != TypeChoice {
		return "", 0, fmt.Errorf("jev: answer is %q, not choice", a.Type)
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
