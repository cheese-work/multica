package decisiontx

import (
	"fmt"
	"regexp"
	"strings"
)

// CHE-10344 (CHE-7035 R3): the CHE-4869 guard reads only human_steps and
// steps. A decision ask keeps the human step in its question and option text,
// so "Do you re-queue PR 2022 for merge now?" and "May an agent rebase ...?"
// passed it. This guard reads the question and the options for the agent-owned
// merge-queue, re-queue, rebase and re-run-checks steps.
//
// The question is read whole: a question that names one of these steps hands
// it to a human. An option is read only where it addresses the human ("You add
// the PR to the merge queue"), so an option that says what the agent does
// ("I re-run the checks") stays valid.
//
// An ask that names an escalation class in the question or the options stays
// askable: irreversible migration, C00 config or secrets, public publication,
// spend, design PR, release branch, production deploy.
const queueGap = `(?:[^.;\n]|\.\S)`

const queueStep = `\bre-?queu\w*|\bmerge[- ]queue\b|\brebas(?:e|es|ed|ing)\b` +
	`|\bre-?run\b` + queueGap + `{0,40}\b(?:CI|checks?|workflow|jobs?)\b` +
	`|\b(?:CI|checks?|workflow|jobs?)\b` + queueGap + `{0,40}\bre-?run\b`

var (
	questionQueueStep = regexp.MustCompile(`(?i)` + queueStep)
	optionQueueStep   = regexp.MustCompile(`(?i)\byou\b` + queueGap + `{0,60}(?:` + queueStep + `)`)
	notIrreversible   = regexp.MustCompile(`(?i)\b(?:not|non)[- ]irreversible\b|n't irreversible\b`)
	escalationClass   = regexp.MustCompile(`(?i)\birreversible\b|\bsecrets?\b|\bC00\b|\bpublic(?:ly)?\b|\bpublish\w*|\bspend\b|\bspent\b|\bbudget\b|\binvoice\b|\$\s?\d|\busd\b|\bdollars?\b|\bpay\b|\bpaid\b|\bdesign\b|\brelease branch\b|\brelease/\S|\bproduction\b|\bprod deploy\b`)
)

// namesEscalationClass reports whether the question or an option names one of
// the five escalation classes (spend, production deploy, publication,
// irreversible action, open product choice) or a never-agent-merged class.
func (ask Ask) namesEscalationClass() bool {
	parts := []string{ask.Decision}
	for _, option := range ask.Options {
		parts = append(parts, option.Label, option.WhatHappens)
	}
	text := notIrreversible.ReplaceAllString(strings.Join(parts, "\n"), " ")
	return escalationClass.MatchString(text)
}

// validateAgentOwnedQuestion refuses a decision ask whose question or option
// hands a human an agent-owned merge-queue, re-queue, rebase or re-run step.
func (ask Ask) validateAgentOwnedQuestion() error {
	if ask.namesEscalationClass() {
		return nil
	}
	if questionQueueStep.MatchString(ask.Decision) {
		return &AskError{"ask.decision", agentOwnedMessage}
	}
	for index, option := range ask.Options {
		for _, field := range []struct{ name, value string }{{"label", option.Label}, {"what_happens", option.WhatHappens}} {
			if optionQueueStep.MatchString(field.value) {
				return &AskError{fmt.Sprintf("ask.options[%d].%s", index, field.name), agentOwnedMessage}
			}
		}
	}
	return nil
}
