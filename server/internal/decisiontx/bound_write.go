package decisiontx

import "fmt"

// ApproveWriteLabel is the one option label that approves a decision-bound
// write. The server accepts only a card answer whose text is exactly this label.
const ApproveWriteLabel = "Approve write"

// BoundWriteDecision is the ask's decision text for a decision-bound write. It
// names the field and both hashes; the server requires after_sha256 to appear
// in the ask.
func BoundWriteDecision(field, beforeSHA, afterSHA string) string {
	return fmt.Sprintf("Apply this change to %s?\nbefore_sha256: %s\nafter_sha256: %s", field, beforeSHA, afterSHA)
}
