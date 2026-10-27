package decisiontx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/multica-ai/multica/server/internal/util"
)

type AskOption struct {
	Label       string   `json:"label"`
	WhatHappens string   `json:"what_happens"`
	Pros        []string `json:"pros"`
	Cons        []string `json:"cons"`
	CostOrTime  string   `json:"cost_or_time"`
}

type Ask struct {
	ContextELI10     string      `json:"context_eli10"`
	AgentDid         string      `json:"agent_did"`
	Decision         string      `json:"decision"`
	StakesIfWrong    string      `json:"stakes_if_wrong"`
	Options          []AskOption `json:"options"`
	Recommendation   string      `json:"recommendation"`
	DefaultIfNoReply string      `json:"default_if_no_reply"`
	SourceIssueID    string      `json:"source_issue_id"`
	SourceCommentID  string      `json:"source_comment_id"`
}

// AskError carries a fixed, field-named message. It never echoes decoder text,
// Go type names or input values, so it is safe to return to a caller.
type AskError struct{ Field, Problem string }

func (err *AskError) Error() string { return err.Field + " " + err.Problem }

const askOptionShape = "must be an object with label, what_happens, pros, cons, cost_or_time"

var (
	askFields               = []string{"context_eli10", "agent_did", "decision", "stakes_if_wrong", "options", "recommendation", "default_if_no_reply", "source_issue_id", "source_comment_id"}
	optionFields            = []string{"label", "what_happens", "pros", "cons", "cost_or_time"}
	safeKey                 = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	askMultiAnswer          = regexp.MustCompile(`(?i)\banswer\s+(?:1\s+to\s+5|1\s*[-–]\s*5|per\s+number|each|all)\b`)
	askNumberedDecisions    = regexp.MustCompile(`(?i)\bdecisions?\s+\d+(?:\s*(?:,|and|to|-)\s*\d+)+\b`)
	askNumberedAnswers      = regexp.MustCompile(`(?i)\b(?:numbered\s+answers?|answer\s+per\s+number)\b`)
	askBareUUID             = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}\b`)
	askLongHex              = regexp.MustCompile(`(?i)\b[0-9a-f]{7,}\b`)
	askInlineNumberedMarker = regexp.MustCompile(`(?:^|[ \t])(\d+)[.)][ \t]+`)
)

func errAskObject() error { return &AskError{"ask", "must be one JSON object"} }

// DecodeAsk is the one decoder for the CLI and the server. It then validates.
func DecodeAsk(raw []byte) (Ask, error) {
	var ask Ask
	fields, err := decodeObject(raw)
	if err != nil {
		return ask, errAskObject()
	}
	if err := rejectUnknown("ask", fields, askFields); err != nil {
		return ask, err
	}
	for _, field := range []struct {
		name string
		into *string
	}{
		{"context_eli10", &ask.ContextELI10}, {"agent_did", &ask.AgentDid}, {"decision", &ask.Decision},
		{"stakes_if_wrong", &ask.StakesIfWrong}, {"recommendation", &ask.Recommendation},
		{"default_if_no_reply", &ask.DefaultIfNoReply}, {"source_issue_id", &ask.SourceIssueID}, {"source_comment_id", &ask.SourceCommentID},
	} {
		if err := decodeString("ask."+field.name, fields[field.name], field.into); err != nil {
			return ask, err
		}
	}
	if err := decodeOptions(fields["options"], &ask.Options); err != nil {
		return ask, err
	}
	return ask, ask.Validate()
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, errAskObject()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errAskObject()
	}
	return fields, nil
}

func rejectUnknown(prefix string, fields map[string]json.RawMessage, known []string) error {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if slices.Contains(known, key) {
			continue
		}
		if safeKey.MatchString(key) {
			return &AskError{prefix + "." + key, "is not a known field"}
		}
		return &AskError{prefix, "has an unknown field"}
	}
	return nil
}

func decodeString(field string, raw json.RawMessage, into *string) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return &AskError{field, "must be a string"}
	}
	return nil
}

func decodeStrings(field string, raw json.RawMessage, into *[]string) error {
	if len(raw) == 0 {
		return nil
	}
	var items []*string
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{field, "must be a list of strings"}
	}
	list := make([]string, len(items))
	for index, item := range items {
		if item == nil {
			return &AskError{field, "must be a list of strings"}
		}
		list[index] = *item
	}
	*into = list
	return nil
}

func decodeOptions(raw json.RawMessage, into *[]AskOption) error {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{"ask.options", "must be a list"}
	}
	for index, item := range items {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return &AskError{prefix, askOptionShape}
		}
		if err := rejectUnknown(prefix, fields, optionFields); err != nil {
			return err
		}
		var option AskOption
		for _, field := range []struct {
			name string
			into *string
		}{{"label", &option.Label}, {"what_happens", &option.WhatHappens}, {"cost_or_time", &option.CostOrTime}} {
			if err := decodeString(prefix+"."+field.name, fields[field.name], field.into); err != nil {
				return err
			}
		}
		if err := decodeStrings(prefix+".pros", fields["pros"], &option.Pros); err != nil {
			return err
		}
		if err := decodeStrings(prefix+".cons", fields["cons"], &option.Cons); err != nil {
			return err
		}
		*into = append(*into, option)
	}
	return nil
}

func (ask Ask) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"context_eli10", ask.ContextELI10}, {"agent_did", ask.AgentDid}, {"decision", ask.Decision},
		{"stakes_if_wrong", ask.StakesIfWrong},
		{"recommendation", ask.Recommendation}, {"default_if_no_reply", ask.DefaultIfNoReply},
	} {
		if strings.TrimSpace(field.value) == "" {
			return &AskError{"ask." + field.name, "is required"}
		}
	}
	if len(ask.Options) < 2 {
		return &AskError{"ask.options", "requires at least two structured options"}
	}
	labels := make(map[string]bool)
	for index, option := range ask.Options {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		for _, field := range []struct{ name, value string }{
			{"label", option.Label}, {"what_happens", option.WhatHappens}, {"cost_or_time", option.CostOrTime},
		} {
			if strings.TrimSpace(field.value) == "" {
				return &AskError{prefix + "." + field.name, "is required"}
			}
		}
		for _, list := range []struct {
			name  string
			items []string
		}{{"pros", option.Pros}, {"cons", option.Cons}} {
			if !slices.ContainsFunc(list.items, func(item string) bool { return strings.TrimSpace(item) != "" }) {
				return &AskError{prefix + "." + list.name, "requires at least one nonblank item"}
			}
		}
		label := strings.ToLower(strings.TrimSpace(option.Label))
		if labels[label] {
			return &AskError{prefix + ".label", "must differ from the other labels"}
		}
		labels[label] = true
	}
	if asksMultipleQuestions(ask) {
		return &AskError{"ask.decision", "asks more than one question; split into one request per question"}
	}
	for _, field := range ask.visibleProseFields() {
		if err := validateVisibleProse(field.name, field.value); err != nil {
			return err
		}
	}
	if plainWordCount(ask.AgentDid) < 4 {
		return &AskError{"ask.agent_did", "must say in at least one plain sentence what the agent did; an ID alone is not enough"}
	}
	for _, field := range []struct{ name, value string }{
		{"source_issue_id", ask.SourceIssueID}, {"source_comment_id", ask.SourceCommentID},
	} {
		if _, err := util.ParseUUID(field.value); err != nil {
			return &AskError{"ask." + field.name, "must be a UUID"}
		}
	}
	return nil
}

func asksMultipleQuestions(ask Ask) bool {
	if askMultiAnswer.MatchString(ask.Decision) || askNumberedDecisions.MatchString(ask.Decision) || strings.Count(ask.Decision, "?") >= 2 || hasMultipleNumberedQuestions(ask.Decision) {
		return true
	}
	for _, option := range ask.Options {
		if askNumberedAnswers.MatchString(option.Label) || askNumberedAnswers.MatchString(option.WhatHappens) {
			return true
		}
	}
	return false
}

func hasMultipleNumberedQuestions(text string) bool {
	markers := askInlineNumberedMarker.FindAllStringSubmatchIndex(text, -1)
	questions := 0
	for index, marker := range markers {
		end := len(text)
		if index+1 < len(markers) {
			end = markers[index+1][0]
		}
		if strings.HasSuffix(strings.TrimSpace(text[marker[1]:end]), "?") {
			questions++
			if questions == 2 {
				return true
			}
		}
	}
	return false
}

func (ask Ask) visibleProseFields() []struct{ name, value string } {
	fields := []struct{ name, value string }{
		{"ask.context_eli10", ask.ContextELI10},
		{"ask.decision", ask.Decision},
		{"ask.stakes_if_wrong", ask.StakesIfWrong},
		{"ask.recommendation", ask.Recommendation},
		{"ask.default_if_no_reply", ask.DefaultIfNoReply},
	}
	for index, option := range ask.Options {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		fields = append(fields,
			struct{ name, value string }{prefix + ".label", option.Label},
			struct{ name, value string }{prefix + ".what_happens", option.WhatHappens},
		)
		for itemIndex, item := range option.Pros {
			fields = append(fields, struct{ name, value string }{fmt.Sprintf("%s.pros[%d]", prefix, itemIndex), item})
		}
		for itemIndex, item := range option.Cons {
			fields = append(fields, struct{ name, value string }{fmt.Sprintf("%s.cons[%d]", prefix, itemIndex), item})
		}
		fields = append(fields, struct{ name, value string }{prefix + ".cost_or_time", option.CostOrTime})
	}
	return fields
}

func validateVisibleProse(field, text string) error {
	if hasInlineNumberedList(text) {
		return &AskError{field, "has an inline numbered list; put each item on its own line as a Markdown list"}
	}
	if askBareUUID.MatchString(stripMarkdownLinkDestinations(text)) {
		return &AskError{field, "has a bare ID; link it as [what it is](link)"}
	}
	return nil
}

func hasInlineNumberedList(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		markers := askInlineNumberedMarker.FindAllStringSubmatchIndex(line, -1)
		for index, marker := range markers {
			if line[marker[2]:marker[3]] != "1" || strings.TrimSpace(line[marker[1]:]) == "" {
				continue
			}
			for _, next := range markers[index+1:] {
				if line[next[2]:next[3]] == "2" && strings.TrimSpace(line[marker[1]:next[0]]) != "" && strings.TrimSpace(line[next[1]:]) != "" {
					return true
				}
			}
		}
	}
	return false
}

func stripMarkdownLinkDestinations(text string) string {
	var output strings.Builder
	output.Grow(len(text))
	bracketDepth := 0
	for index := 0; index < len(text); {
		if text[index] == '[' {
			bracketDepth++
			output.WriteByte(text[index])
			index++
			continue
		}
		if text[index] == ']' && bracketDepth > 0 && index+1 < len(text) && text[index+1] == '(' {
			if end := markdownLinkDestinationEnd(text, index+2); end >= 0 {
				output.WriteByte(']')
				bracketDepth--
				index = end + 1
				continue
			}
		}
		if text[index] == ']' && bracketDepth > 0 {
			bracketDepth--
		}
		output.WriteByte(text[index])
		index++
	}
	return output.String()
}

func markdownLinkDestinationEnd(text string, start int) int {
	depth := 1
	for index := start; index < len(text); index++ {
		switch text[index] {
		case '\\':
			index++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func plainWordCount(text string) int {
	text = lintLinks.ReplaceAllString(text, " ")
	text = askBareUUID.ReplaceAllString(text, " ")
	text = askLongHex.ReplaceAllString(text, " ")
	count := 0
	for _, word := range strings.Fields(text) {
		if strings.IndexFunc(word, unicode.IsLetter) >= 0 {
			count++
		}
	}
	return count
}

func (ask Ask) Content() string {
	var options strings.Builder
	for _, option := range ask.Options {
		fmt.Fprintf(&options, "- **%s**: %s\n", option.Label, option.WhatHappens)
		for _, pro := range option.Pros {
			fmt.Fprintf(&options, "  - Pro: %s\n", pro)
		}
		for _, con := range option.Cons {
			fmt.Fprintf(&options, "  - Con: %s\n", con)
		}
		fmt.Fprintf(&options, "  - Cost or time: %s\n", option.CostOrTime)
	}
	return fmt.Sprintf("**Context**\n%s\n\n**Decision**\n%s\n\n**What is at stake**\n%s\n\n**Options**\n%s\n**Recommendation**\n%s\n\n**Default if no reply**\n%s\n\n<details>\n<summary>Details</summary>\n\n**Agent did**\n%s\n\n</details>\n\nInvestigate in issue: [Issue](mention://issue/%s) · [Comment](/issue/%s#comment-%s)",
		ask.ContextELI10, ask.Decision, ask.StakesIfWrong, options.String(), ask.Recommendation, ask.DefaultIfNoReply, ask.AgentDid, ask.SourceIssueID, ask.SourceIssueID, ask.SourceCommentID)
}
