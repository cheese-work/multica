package decisiontx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// CHE-5993 card lint (decision-screen plan r4, section 3.2): a card whose option
// text is weak, markdown-heavy or points at the recommendation is refused with a
// reason code and a field path. English and Vietnamese patterns; agents write both.
// Card text is matched, never the typed fields of CHE-5902.

const SettingCardLintMode = "decision_card_lint_mode"

// escalationClasses never carry a default that names an option.
var escalationClasses = []string{"spend", "deploy", "publish", "irreversible", "product_direction"}

// CardLintModeFromSettings reads decision_card_lint_mode (off, warn or block; default
// off) from a workspace.settings value. A malformed value is an error: callers must not guess.
func CardLintModeFromSettings(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "off", nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", fmt.Errorf("workspace settings: %w", err)
	}
	value, ok := settings[SettingCardLintMode]
	if !ok {
		return "off", nil
	}
	var mode *string // null is not a mode
	if err := json.Unmarshal(value, &mode); err != nil || mode == nil || *mode != "off" && *mode != "warn" && *mode != "block" {
		return "", fmt.Errorf("%s must be off, warn or block", SettingCardLintMode)
	}
	return *mode, nil
}

// ponytail: patterns match the cost phrasing seen in real cards; add one when a new phrasing slips through.
var (
	// A con that is only the human's time or one more check: the whole statement,
	// so a real cost beside the time ("... and deletes the backup") is not refused.
	weakCon = regexp.MustCompile(`(?i)^(?:(?:it )?(?:takes|costs|needs|requires|mất|tốn) )?(?:(?:about |around |~)?\d+ ?(?:minutes?|mins?|seconds?|hours?|phút|giây|giờ)(?: of)?(?: your (?:time|attention)| của bạn)?|your (?:time|attention)|thời gian của bạn|(?:one|1|an?) (?:more|extra|additional) (?:check|review|step|approval|click)|thêm (?:một|1) lần (?:kiểm tra|duyệt|xem lại|review))$`)

	mdTableRule = regexp.MustCompile(`(?m)^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$`)
	mdHeading   = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s`)
	mdImage     = regexp.MustCompile(`!\[[^\]]*\]\(`)
	mdHTML      = regexp.MustCompile(`</?[A-Za-z][^>]*>|<!--`)
	mdFence     = regexp.MustCompile("(?m)^\\s*(?:```|~~~)")
	inlineCode  = regexp.MustCompile("`[^`\n]*`")

	leakWord = regexp.MustCompile(`(?i)\brecommended\b|khuyến nghị|\(rec\)`)
	boldSpan = regexp.MustCompile(`\*\*[^*]+\*\*|__[^_]+__`)

	noActionPhrase = regexp.MustCompile(`(?i)no action until you pick`)
)

// optionTexts yields the option fields the markdown rule reads, with their paths.
func optionTexts(index int, option AskOption) (fields []string, texts []string) {
	prefix := fmt.Sprintf("ask.options[%d]", index)
	fields = append(fields, prefix+".label", prefix+".what_happens")
	texts = append(texts, option.Label, option.WhatHappens)
	for _, list := range []struct {
		name  string
		items []string
	}{{"pros", option.Pros}, {"cons", option.Cons}} {
		for _, item := range list.items {
			fields = append(fields, prefix+"."+list.name)
			texts = append(texts, item)
		}
	}
	return fields, texts
}

// CheckCard applies the card lint to an option card. class is the escalation class
// of the request. Mode off checks nothing; warn returns one hint per finding; block
// returns the first finding as an AskError whose Problem starts with the reason code.
func (ask Ask) CheckCard(mode, class string) (hints []string, err error) {
	if mode == "off" {
		return nil, nil
	}
	var found []AskError
	add := func(field, code, problem string) { found = append(found, AskError{field, code + ": " + problem}) }

	boldLabels := -1
	for index, option := range ask.Options {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		if weak(option.Cons) {
			add(prefix+".cons", "weak_cons", "every con is only your time or one more check; name a real cost of this option")
		}
		fields, texts := optionTexts(index, option)
		markdownFound, leakFound := false, false
		for at, text := range texts {
			if kind := markdownKind(text); kind != "" && !markdownFound {
				add(fields[at], "option_markdown", kind+" is not allowed in option text; use bold, italic, inline code, lists or links")
				markdownFound = true
			}
			if leakWord.MatchString(text) && !leakFound {
				add(fields[at], "option_leak", "option text must not mark the recommendation; recommended_option names it")
				leakFound = true
			}
		}
		if boldSpan.MatchString(option.Label) {
			if boldLabels == -1 {
				boldLabels = index
			} else {
				boldLabels = -2 // more than one bold label: not a leak
			}
		}
	}
	if boldLabels >= 0 && len(ask.Options) > 1 {
		add(fmt.Sprintf("ask.options[%d].label", boldLabels), "option_leak", "bold on one label only marks the recommendation; bold every label or none")
	}
	if slices.Contains(escalationClasses, class) {
		if label, ok := ask.defaultNamesOption(); ok {
			add("ask.default_if_no_reply", "default_leak", fmt.Sprintf("names the option %q; for class %s the default is \"No action until you pick.\"", label, class))
		}
	}
	if mode == "block" {
		if len(found) > 0 {
			return nil, &found[0]
		}
		return nil, nil
	}
	for _, finding := range found {
		hints = append(hints, finding.Error())
	}
	return hints, nil
}

// weak reports a con list whose every nonblank item is only the human's time or one more check.
func weak(cons []string) bool {
	seen := false
	for _, con := range cons {
		if strings.TrimSpace(con) == "" {
			continue
		}
		if !weakCon.MatchString(strings.TrimRight(norm.NFC.String(strings.TrimSpace(con)), ".! ")) {
			return false
		}
		seen = true
	}
	return seen
}

// markdownKind names the first forbidden markup in text, or "".
func markdownKind(text string) string {
	if mdFence.MatchString(text) {
		return "a fenced code or Mermaid block"
	}
	text = inlineCode.ReplaceAllString(text, " ")
	switch {
	case mdTableRule.MatchString(text):
		return "a table"
	case mdHeading.MatchString(text):
		return "a heading"
	case mdImage.MatchString(text):
		return "an image"
	case mdHTML.MatchString(text):
		return "HTML"
	}
	return ""
}

// defaultNamesOption finds an option label in default_if_no_reply, ignoring the allowed phrase.
func (ask Ask) defaultNamesOption() (string, bool) {
	text := noActionPhrase.ReplaceAllString(ask.DefaultIfNoReply, " ")
	for _, option := range ask.Options {
		label := strings.TrimSpace(option.Label)
		if label == "" {
			continue
		}
		if regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])` + regexp.QuoteMeta(label) + `(?:$|[^\p{L}\p{N}])`).MatchString(text) {
			return label, true
		}
	}
	return "", false
}

// MentionHuman appends a mention of the asked human to a card's issue comment
// unless it already carries one (plan section 3.2 mention_missing: the server adds
// the mention, it does not refuse). added reports whether it changed content.
func MentionHuman(content, name, id string) (string, bool) {
	if strings.Contains(content, "mention://member/"+id+")") {
		return content, false
	}
	name = strings.NewReplacer("[", "", "]", "").Replace(strings.TrimSpace(name))
	return content + "\n\n[@" + name + "](mention://member/" + id + ")", true
}
