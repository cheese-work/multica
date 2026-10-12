package governance

import (
	"fmt"

	"github.com/multica-ai/multica/server/pkg/jev"
)

// Question IDs. These are code keys, not shown to the model — every
// instruction below names its subject explicitly instead.
const (
	questionHandoffKind = "handoff_kind"
	questionNextOwner   = "next_owner"
	questionEvidence    = "evidence"
	questionCorrection  = "correction"
)

// handoff_kind options.
const (
	handoffAgentWork        = "agent_work"
	handoffAgentPreparation = "agent_preparation"
	handoffHumanDecision    = "human_decision"
	handoffNoFollowup       = "no_followup"
	handoffUnclear          = "unclear"
)

// correction options.
const (
	correctionMentionOwner         = "mention_owner"
	correctionReturnMechanicalStep = "return_mechanical_step_to_assignee"
	correctionNoCorrection         = "no_correction"
	correctionUnclear              = "unclear"
)

// Shared sentinel options offered on next_owner and evidence.
const (
	optionNone      = "none"
	optionAmbiguous = "ambiguous"
)

// questionSpec is the fully-resolved set of four questions for one
// evaluation: the jev.Question values sent on the wire, the offered label
// set for each (used to strictly validate the returned answer), and the
// label-to-index maps used to resolve a winning candidate or span back to
// the Input slice it came from.
type questionSpec struct {
	handoffKindQuestion jev.Question
	nextOwnerQuestion   jev.Question
	evidenceQuestion    jev.Question
	correctionQuestion  jev.Question

	offered map[string][]string

	candidateIndex map[string]int
	spanIndex      map[string]int
}

func (s questionSpec) questions() map[string]jev.Question {
	return map[string]jev.Question{
		questionHandoffKind: s.handoffKindQuestion,
		questionNextOwner:   s.nextOwnerQuestion,
		questionEvidence:    s.evidenceQuestion,
		questionCorrection:  s.correctionQuestion,
	}
}

// candidateLabel and spanLabel produce the "candidate_01".."candidate_N" and
// "span_01".."span_N" option labels the spec names. Two digits comfortably
// covers MaxCandidates/MaxSpans (16) while matching the spec's example
// naming exactly.
func candidateLabel(i int) string { return fmt.Sprintf("candidate_%02d", i+1) }
func spanLabel(i int) string      { return fmt.Sprintf("span_%02d", i+1) }

// buildQuestionSpec composes the one request's four bounded Choice
// questions and their citation map (the label-to-Span/Candidate index used
// to resolve the model's answer, and later the executor's citation) from
// Input. Candidates and Spans must already be within MaxCandidates/MaxSpans
// — Evaluate checks that before calling this.
func buildQuestionSpec(in Input) questionSpec {
	candidateOptions := make(map[string]string, len(in.Candidates)+2)
	candidateIndex := make(map[string]int, len(in.Candidates))
	candidateLabels := make([]string, 0, len(in.Candidates)+2)
	for i, c := range in.Candidates {
		label := candidateLabel(i)
		candidateIndex[label] = i
		candidateLabels = append(candidateLabels, label)
		candidateOptions[label] = fmt.Sprintf("%s — %s", c.DisplayName, c.Description)
	}
	candidateOptions[optionNone] = "No agent-owned next action is needed."
	candidateOptions[optionAmbiguous] = "More than one candidate could plausibly own this; do not guess."
	candidateLabels = append(candidateLabels, optionNone, optionAmbiguous)

	spanOptions := make(map[string]string, len(in.Spans)+2)
	spanIndex := make(map[string]int, len(in.Spans))
	spanLabels := make([]string, 0, len(in.Spans)+2)
	for i, sp := range in.Spans {
		label := spanLabel(i)
		spanIndex[label] = i
		spanLabels = append(spanLabels, label)
		spanOptions[label] = sp.Text
	}
	spanOptions[optionNone] = "No offered passage contains an actionable next-work request."
	spanOptions[optionAmbiguous] = "More than one passage could plausibly be the actionable request."
	spanLabels = append(spanLabels, optionNone, optionAmbiguous)

	handoffOptions := map[string]string{
		handoffAgentWork:        "The comment requests concrete agent work.",
		handoffAgentPreparation: "Routine agent work must happen before a requested human decision.",
		handoffHumanDecision:    "Only a genuine decision reserved for a person is being requested.",
		handoffNoFollowup:       "The comment is a report, thanks, FYI, or a quoted example with no follow-up.",
		handoffUnclear:          "The requested next action cannot be determined.",
	}
	correctionOptions := map[string]string{
		correctionMentionOwner:         "Deliver the named agent work to its resolved owner.",
		correctionReturnMechanicalStep: "Delegate the routine preparation step to the current accountable assignee.",
		correctionNoCorrection:         "No correction should be made.",
		correctionUnclear:              "It is not clear which correction, if any, applies.",
	}

	return questionSpec{
		handoffKindQuestion: jev.NewChoice(
			"What next action does the source comment request in this issue?",
			handoffOptions,
		),
		nextOwnerQuestion: jev.NewChoice(
			"If an agent-owned next action is needed, which listed candidate is responsible for it under the applicable rule and recorded ownership?",
			candidateOptions,
		),
		evidenceQuestion: jev.NewChoice(
			"Which offered source passage contains the actionable next-work request, including a request that mistakenly gives routine agent work to a person?",
			spanOptions,
		),
		correctionQuestion: jev.NewChoice(
			"Which permitted response addresses this comment's next-work request?",
			correctionOptions,
		),
		offered: map[string][]string{
			questionHandoffKind: {handoffAgentWork, handoffAgentPreparation, handoffHumanDecision, handoffNoFollowup, handoffUnclear},
			questionNextOwner:   candidateLabels,
			questionEvidence:    spanLabels,
			questionCorrection:  {correctionMentionOwner, correctionReturnMechanicalStep, correctionNoCorrection, correctionUnclear},
		},
		candidateIndex: candidateIndex,
		spanIndex:      spanIndex,
	}
}
