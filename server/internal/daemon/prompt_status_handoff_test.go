package daemon

import (
	"strings"
	"testing"
)

// CHE-5485: a status_handoff trigger comment is labelled handoff context and
// never turns the run into a comment-reply run.
func TestBuildPromptBody_StatusHandoffComment(t *testing.T) {
	task := Task{
		IssueID:               "issue-1",
		TriggerCommentID:      "comment-1",
		TriggerKind:           "status_handoff",
		TriggerCommentContent: "step brief line 1\nline 2",
	}
	got := buildPromptBody(task, "claude")
	for _, want := range []string{"Handoff context", "> step brief line 1", "> line 2", "multica issue get issue-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"[NEW COMMENT]", "--parent comment-1"} {
		if strings.Contains(got, bad) {
			t.Errorf("prompt has comment-run marker %q:\n%s", bad, got)
		}
	}
	if targets := commentReplyThreads(task); targets != nil {
		t.Errorf("commentReplyThreads = %v, want nil", targets)
	}

	task.TriggerKind = ""
	if got := buildPromptBody(task, "claude"); !strings.Contains(got, "[NEW COMMENT]") {
		t.Errorf("plain comment trigger lost its comment prompt:\n%s", got)
	}
}
