package decisiontx

import "regexp"

// askCue marks a comment as a request to a person: "please", "you need to",
// "Cheese,", "steps for you", and the Vietnamese and Chinese forms.
var askCue = regexp.MustCompile(`(?i)\b(?:please|you\s+(?:need|must|should|have)\s+to|can\s+you|could\s+you|cheese|human\s+action|owner\s+action|steps\s+for\s+you)\b|vui lòng|请`)

// CheckCommentAsk refuses a plain comment that asks a person to act and cites
// a UI path, a repo file, a command or a placeholder (CHE-5753). A comment has
// no observed, files or dry_run fields, so the evidence is always missing:
// the caller sends the ask as a decision request instead.
func CheckCommentAsk(content string) error {
	if !askCue.MatchString(content) {
		return nil
	}
	return askEvidence{}.check("comment", "comment", []askText{{field: "comment", text: content}})
}
