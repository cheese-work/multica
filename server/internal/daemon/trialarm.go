package daemon

import (
	"crypto/sha256"
	"log/slog"
)

// TrialArm returns the cost-trial arm for a root issue key (CHE-6498 S6a):
// the sha256 digest of the key read as a big-endian integer, mod 2. For a
// big-endian integer that is the parity of the last digest byte. 0 is control,
// 1 is treatment. No lever reads the arm yet, so it changes no behaviour.
func TrialArm(rootKey string) int {
	sum := sha256.Sum256([]byte(rootKey))
	return int(sum[len(sum)-1] & 1)
}

// RootIssueKey is the identifier of the root of this run's issue tree.
// Children, grandchildren and reruns of one issue share the root's key, so
// they share its arm. ok is false when the root cannot be known: a task
// without an issue, or a sub-issue claimed from a server too old to send
// RootIssueIdentifier. The parent is not a safe substitute, since a
// grandchild would land in the wrong arm.
func (t Task) RootIssueKey() (string, bool) {
	if t.RootIssueIdentifier != "" {
		return t.RootIssueIdentifier, true
	}
	if t.ParentIssue == nil && t.IssueIdentifier != "" {
		return t.IssueIdentifier, true
	}
	return "", false
}

// logTrialArm writes one line per issue-bound run with the issue key, the root
// key and the arm. Chat and other non-issue runs have no issue key and log
// nothing.
func logTrialArm(log *slog.Logger, task Task) {
	if task.IssueIdentifier == "" {
		return
	}
	root, ok := task.RootIssueKey()
	if !ok {
		log.Info("trial arm", "issue", task.IssueIdentifier, "root", "unknown", "arm", "unknown")
		return
	}
	log.Info("trial arm", "issue", task.IssueIdentifier, "root", root, "arm", TrialArm(root))
}
