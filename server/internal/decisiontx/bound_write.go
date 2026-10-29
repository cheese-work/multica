package decisiontx

import "fmt"

// ApproveWriteLabel is the one option label that approves a decision-bound
// write. The server accepts only a card answer whose text is exactly this label.
const ApproveWriteLabel = "Approve write"

// BoundWriteDecision is the ask's decision text for a decision-bound write. It
// names the field and both hashes; the server requires its before_sha256 and
// after_sha256 lines to be the write's hashes.
func BoundWriteDecision(field, beforeSHA, afterSHA string) string {
	return fmt.Sprintf("%sbefore_sha256: %s\nafter_sha256: %s", BoundWriteHeading(field), beforeSHA, afterSHA)
}

// BoundWriteHeading is the first line of the ask, with its newline. The server
// requires it to name the write's field, so an approval cannot be reused on
// another field.
func BoundWriteHeading(field string) string {
	return fmt.Sprintf("Apply this change to %s?\n", field)
}
