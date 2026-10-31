package decisiontx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// StepValue is one literal a human must use. Value is the literal; Source says
// where to read it when the literal cannot be written here.
type StepValue struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// HumanStep is one action a human performs, with the proof it worked.
type HumanStep struct {
	Action     string      `json:"action"`
	Command    string      `json:"command"`
	Values     []StepValue `json:"values"`
	VerifyDone string      `json:"verify_done"`
}

const (
	stepShape = "must be an object with action, command, values, verify_done"
	uuidPat   = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
)

var (
	stepFields   = []string{"action", "command", "values", "verify_done"}
	valueFields  = []string{"name", "value", "source"}
	upperName    = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b`)
	placeholder  = regexp.MustCompile(`<[^<>\s]+>`)
	bareUUID     = regexp.MustCompile(uuidPat)
	mentionLink  = regexp.MustCompile(`\[[^\]]*\]\(mention://[^)]*\)`)
	implicitWord = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(similarly|likewise|same as|etc\.|tương tự|v\.v\.?)(?:$|[^\p{L}])`)
	wantsAction  = regexp.MustCompile(`(?i)\b(?:run|merge|set|click|create)\b`)
	mergeWord    = regexp.MustCompile(`(?i)\bmerge\b`)
	readyState   = regexp.MustCompile(`(?i)\b(?:ready|draft)\b`)
	ciState      = regexp.MustCompile(`(?i)\b(?:ci|checks?)\b`)
)

func decodeSteps(raw json.RawMessage, into *[]HumanStep) error {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{"ask.human_steps", "must be a list"}
	}
	for index, item := range items {
		prefix := fmt.Sprintf("ask.human_steps[%d]", index)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return &AskError{prefix, stepShape}
		}
		if err := rejectUnknown(prefix, fields, stepFields); err != nil {
			return err
		}
		var step HumanStep
		for _, field := range []struct {
			name string
			into *string
		}{{"action", &step.Action}, {"command", &step.Command}, {"verify_done", &step.VerifyDone}} {
			if err := decodeString(prefix+"."+field.name, fields[field.name], field.into); err != nil {
				return err
			}
		}
		if err := decodeValues(prefix+".values", fields["values"], &step.Values); err != nil {
			return err
		}
		*into = append(*into, step)
	}
	return nil
}

func decodeValues(field string, raw json.RawMessage, into *[]StepValue) error {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{field, "must be a list"}
	}
	for index, item := range items {
		prefix := fmt.Sprintf("%s[%d]", field, index)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return &AskError{prefix, "must be an object with name, value, source"}
		}
		if err := rejectUnknown(prefix, fields, valueFields); err != nil {
			return err
		}
		var value StepValue
		for _, f := range []struct {
			name string
			into *string
		}{{"name", &value.Name}, {"value", &value.Value}, {"source", &value.Source}} {
			if err := decodeString(prefix+"."+f.name, fields[f.name], f.into); err != nil {
				return err
			}
		}
		*into = append(*into, value)
	}
	return nil
}

func blank(text string) bool { return strings.TrimSpace(text) == "" }

func hasBareUUID(text string) bool {
	return bareUUID.MatchString(mentionLink.ReplaceAllString(text, " "))
}

func (ask Ask) validateHumanSteps() error {
	if len(ask.HumanSteps) == 0 {
		return nil
	}
	for index, step := range ask.HumanSteps {
		if err := step.validate(fmt.Sprintf("ask.human_steps[%d]", index)); err != nil {
			return err
		}
	}
	if !slices.ContainsFunc(ask.Probes, func(probe string) bool { return !blank(probe) }) {
		return &AskError{"ask.probes", "requires at least one item"}
	}
	if blank(ask.AfterDone) {
		return &AskError{"ask.after_done", "is required"}
	}
	if hasBareUUID(ask.AfterDone) {
		return &AskError{"ask.after_done", "has a bare UUID; use a name or a mention link"}
	}
	return nil
}

func (step HumanStep) validate(prefix string) error {
	for _, field := range []struct{ name, value string }{{"action", step.Action}, {"verify_done", step.VerifyDone}} {
		if blank(field.value) {
			return &AskError{prefix + "." + field.name, "is required"}
		}
	}
	for index, value := range step.Values {
		at := fmt.Sprintf("%s.values[%d]", prefix, index)
		switch {
		case blank(value.Name):
			return &AskError{at + ".name", "is required"}
		case blank(value.Value) && blank(value.Source):
			return &AskError{at, "requires a value or a source"}
		case blank(value.Source) && (placeholder.MatchString(value.Value) || strings.ContainsAny(value.Value, "…") || strings.Contains(value.Value, "...")):
			return &AskError{at + ".value", "is a placeholder; give the literal or a source"}
		}
	}
	text := step.Action + "\n" + step.Command
	for _, name := range append(upperName.FindAllString(text, -1), placeholder.FindAllString(text, -1)...) {
		if !step.declares(name) {
			return &AskError{prefix, "names " + name + " without a values entry"}
		}
	}
	for _, field := range []struct{ name, value string }{{"action", step.Action}, {"command", step.Command}} {
		if match := implicitWord.FindStringSubmatch(field.value); match != nil {
			return &AskError{prefix + "." + field.name, "leaves values implicit (" + strings.ToLower(match[1]) + "); write each one"}
		}
	}
	for _, field := range []struct{ name, value string }{{"action", step.Action}, {"verify_done", step.VerifyDone}} {
		if hasBareUUID(field.value) {
			return &AskError{prefix + "." + field.name, "has a bare UUID; use a name or a mention link"}
		}
	}
	return nil
}

// declares reports whether a values name carries the identifier as a whole
// token: MULTICA_TOKEN_BACKUP does not declare MULTICA_TOKEN. A descriptive
// suffix after a space or bracket is allowed.
func (step HumanStep) declares(name string) bool {
	whole := regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + regexp.QuoteMeta(strings.Trim(name, "<>")) + `(?:$|[^A-Za-z0-9_])`)
	return slices.ContainsFunc(step.Values, func(value StepValue) bool { return whole.MatchString(value.Name) })
}

// lintHumanSteps adds advisory hints; it never blocks.
func lintHumanSteps(ask Ask) []string {
	var hints []string
	for _, field := range []struct{ name, text string }{
		{"context_eli10", ask.ContextELI10}, {"decision", ask.Decision}, {"stakes_if_wrong", ask.StakesIfWrong},
	} {
		if hasBareUUID(field.text) {
			hints = append(hints, field.name+" has a bare UUID; say who or what it is in plain words")
		}
	}
	asksToAct := wantsAction.MatchString(ask.Decision)
	for index, option := range ask.Options {
		if hasBareUUID(option.WhatHappens) {
			hints = append(hints, fmt.Sprintf("options[%d].what_happens has a bare UUID; say who or what it is in plain words", index))
		}
		asksToAct = asksToAct || wantsAction.MatchString(option.Label+" "+option.WhatHappens)
	}
	if len(ask.HumanSteps) == 0 {
		if asksToAct {
			hints = append(hints, "the decision or an option asks you to run, merge, set, click or create something, but there is no human_steps; list each step")
		}
		return hints
	}
	probes := strings.Join(ask.Probes, "\n")
	for index, step := range ask.HumanSteps {
		for _, value := range step.Values {
			if hasBareUUID(value.Value) && !strings.Contains(strings.ToLower(value.Name), "uuid") {
				hints = append(hints, fmt.Sprintf("human_steps[%d] has a UUID value; say UUID in its name (%s)", index, value.Name))
			}
		}
		if mergeWord.MatchString(step.Action+" "+step.Command) && !(readyState.MatchString(probes) && ciState.MatchString(probes)) {
			hints = append(hints, fmt.Sprintf("human_steps[%d] is a merge step; probes should say if the PR is ready or draft and the CI state", index))
		}
	}
	return hints
}

func fenceFor(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// humanStepSections renders the sections placed between "Default if no reply"
// and Details. Each ends with a blank line; an ask with none renders nothing.
func (ask Ask) humanStepSections() string {
	var out strings.Builder
	if len(ask.HumanSteps) > 0 {
		out.WriteString("**Steps for you**\n")
		for index, step := range ask.HumanSteps {
			fmt.Fprintf(&out, "%d. %s\n", index+1, step.Action)
			if !blank(step.Command) {
				fence := fenceFor(step.Command)
				fmt.Fprintf(&out, "   %s\n   %s\n   %s\n", fence, strings.ReplaceAll(step.Command, "\n", "\n   "), fence)
			}
			for _, value := range step.Values {
				fmt.Fprintf(&out, "   - %s: %s\n", value.Name, valueText(value))
			}
			fmt.Fprintf(&out, "   - Done when: %s\n", step.VerifyDone)
		}
		out.WriteString("\n")
	}
	if len(ask.Probes) > 0 {
		out.WriteString("**Checked first**\n")
		for _, probe := range ask.Probes {
			fmt.Fprintf(&out, "- %s\n", probe)
		}
		out.WriteString("\n")
	}
	if !blank(ask.AfterDone) {
		fmt.Fprintf(&out, "**After you finish**\n%s\n\n", ask.AfterDone)
	}
	return out.String()
}

func valueText(value StepValue) string {
	switch {
	case blank(value.Source):
		return value.Value
	case blank(value.Value):
		return "(source: " + value.Source + ")"
	}
	return value.Value + " (source: " + value.Source + ")"
}
