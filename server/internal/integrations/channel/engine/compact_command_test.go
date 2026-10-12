package engine

import "testing"

func TestCompactCommandRemainsLiteralChannelInput(t *testing.T) {
	for _, input := range []string{"/compact", "/compact focus on evidence", "\n\t/compact focus\nnext action", "/compacted", "/Compact", "please /compact", "hello\n/compact"} {
		if command, ok := ParseControlCommand(input); ok {
			t.Errorf("ParseControlCommand(%q) = %+v; want literal input", input, command)
		}
	}
}
