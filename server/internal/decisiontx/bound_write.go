package decisiontx

import (
	"fmt"
	"strings"
)

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

// passkeyOnlyFields are the bound-write fields a machine credential can no
// longer write (CHE-3482): only a passkey-approved action changes them, so a
// card cannot authorize the write. Each maps to its "multica config propose"
// field and the kind of id it takes.
var passkeyOnlyFields = map[string]struct{ propose, idKind string }{
	"workflow.steps":        {"workflow.steps", "workflow"},
	"workflow.instructions": {"workflow.step.instructions", "workflow"},
	"workspace.context":     {"workspace.context", ""},
	"squad.instructions":    {"squad.instructions", "squad"},
	"agent.instructions":    {"agent.instructions", "agent"},
}

// PasskeyOnlyBoundWriteField returns the field of a bound-write ask whose
// decision text opens with "Apply this change to <field>?" for a passkey-only
// field.
func PasskeyOnlyBoundWriteField(decision string) (string, bool) {
	for field := range passkeyOnlyFields {
		if strings.HasPrefix(decision, strings.TrimSuffix(BoundWriteHeading(field), "\n")) {
			return field, true
		}
	}
	return "", false
}

// PasskeyOnlyCommand is the command that changes field when a card cannot. A
// blank target or statusKey leaves a placeholder.
func PasskeyOnlyCommand(field, target, statusKey string) (string, bool) {
	spec, ok := passkeyOnlyFields[field]
	if !ok {
		return "", false
	}
	command := "multica config propose " + spec.propose
	if spec.idKind != "" {
		if target == "" {
			target = "<" + spec.idKind + "-id>"
		}
		command += " " + target
	}
	if spec.propose == "workflow.step.instructions" {
		if statusKey == "" {
			statusKey = "<key>"
		}
		command += " --status-key " + statusKey
	}
	return command + " --file <path> --out <out>", true
}
