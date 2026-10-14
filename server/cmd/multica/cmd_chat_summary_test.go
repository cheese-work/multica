package main

import "testing"

func TestChatSummarySetCommandRemoved(test *testing.T) {
	if command, remaining, err := chatCmd.Find([]string{"summary", "set"}); err == nil && (command != chatCmd || len(remaining) != 2) {
		test.Fatalf("removed chat summary set command still resolves to %s", command.CommandPath())
	}
}
