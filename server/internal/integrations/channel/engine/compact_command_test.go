package engine

import "testing"

func TestCompactCommandSharedSyntax(t *testing.T) {
	for _, input := range []string{"/compact", "/compact focus on evidence", "\n\t/compact focus\nnext action"} {
		command, ok := ParseControlCommand(input)
		if !ok || command.Kind != ControlCommandCompact {
			t.Errorf("ParseControlCommand(%q) = %+v, %v; want compact", input, command, ok)
		}
	}
	for _, input := range []string{"/compacted", "/Compact", "please /compact", "hello\n/compact"} {
		if command, ok := ParseControlCommand(input); ok {
			t.Errorf("ParseControlCommand(%q) = %+v; want literal input", input, command)
		}
	}
}
