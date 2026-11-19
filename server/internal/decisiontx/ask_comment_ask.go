package decisiontx

import (
	"regexp"
	"slices"
	"strings"
)

// askCue marks a comment as a request to a person: "please", "you need to",
// "Cheese,", "steps for you", and the Vietnamese and Chinese forms.
var askCue = regexp.MustCompile(`(?i)\b(?:please|you\s+(?:need|must|should|have)\s+to|can\s+you|could\s+you|cheese|human\s+action|owner\s+action|steps\s+for\s+you)\b|vui lòng|请`)

// directInstruction marks a sentence, line or list item that opens with an
// imperative UI or shell verb: "Open Settings → X.", "1. Click ...", "Run `x`".
// It needs no cue word: the instruction is the ask. The past tense ("Opened",
// "ran") is a report and does not match.
var directInstruction = regexp.MustCompile(`(?im)(?:^|[.!?:;]\s+)[ \t]*(?:[-*•][ \t]+|\d+[.)][ \t]+)?(?:(?:then|next|now),?[ \t]+)?(?:open|click|run|execute|go[ \t]+to|navigate[ \t]+to|select|choose|paste|press|tap|toggle|enable|disable|download)\b`)

// uiStep is one arrow step of a UI path, one word either side, so words added
// around an old path do not make it look new.
var uiStep = regexp.MustCompile(`([\p{L}\p{N}]+)\s*(?:→|›|»|->|>)\s*([\p{L}\p{N}]+)`)

// CheckCommentAsk refuses a plain comment that asks a person to act and cites
// a UI path, a repo file, a command or a placeholder (CHE-5753). A comment has
// no observed, files or dry_run fields, so the evidence is always missing:
// the caller sends the ask as a decision request instead.
func CheckCommentAsk(content string) error {
	if !askCue.MatchString(content) && !directInstruction.MatchString(content) {
		return nil
	}
	return askEvidence{}.check("comment", "comment", []askText{{field: "comment", text: content}})
}

// AskItems lists the commands, repo files and UI steps a comment cites, the
// things CheckCommentAsk finds unproven. An edit that keeps an ask the comment
// already had adds no item to it.
func AskItems(content string) []string {
	var items []string
	for _, command := range commandsIn(content) {
		items = append(items, "cmd:"+strings.TrimSpace(command))
	}
	for _, name := range repoFiles(content) {
		items = append(items, "file:"+strings.TrimPrefix(name, "./"))
	}
	for _, m := range uiStep.FindAllStringSubmatch(uiText(askText{text: content}), -1) {
		items = append(items, "ui:"+strings.ToLower(m[1]+">"+m[2]))
	}
	return items
}

// AddsAskItem reports whether newContent cites a command, file or UI step that
// oldContent did not.
func AddsAskItem(oldContent, newContent string) bool {
	had := AskItems(oldContent)
	for _, item := range AskItems(newContent) {
		if !slices.Contains(had, item) {
			return true
		}
	}
	return false
}
