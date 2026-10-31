package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/multica-ai/multica/server/internal/humanask"
)

// guardCommentHumanAsk refuses an agent comment that asks a human to act
// without a real member mention link or opening context (CHE-2597). Add
// `Human action: none` to the comment when it asks no one to act.
func guardCommentHumanAsk(content, agentID string) error {
	if agentID == "" {
		return nil
	}
	problems := humanask.Check(content)
	if len(problems) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("agent comment asks a human to act but fails the human-ask check:")
	for _, p := range problems {
		fmt.Fprintf(&b, "\n  line %d: %s", p.Line, p.Message)
	}
	for _, p := range humanask.Warnings(content) {
		fmt.Fprintf(&b, "\n  warning: line %d: %s", p.Line, p.Message)
	}
	b.WriteString("\nFix the lines above (rules: multica-cheese-work-ste-writing human_ask.py), or add the line `Human action: none` if the comment asks no one to act.")
	return fmt.Errorf("%s", b.String())
}

// warnCommentHumanAsk prints the format-rule warnings of an agent comment that
// asks a human to act. The comment is still sent.
func warnCommentHumanAsk(content, agentID string, out io.Writer) {
	if agentID == "" {
		return
	}
	for _, p := range humanask.Warnings(content) {
		fmt.Fprintf(out, "warning: line %d: %s\n", p.Line, p.Message)
	}
}
