package taskfailure

import "testing"

// AGY stores an executor error in its conversation and repeats it on every
// later resumed turn, even after the turn itself succeeds (CHE-1715).
const agyNotFound = "agent executor error: generating and executing: Error 404, Message: Requested entity was not found., Status: NOT_FOUND, Details: []; agy stderr: error: agent executor error: generating and executing: Error 404, Message: Requested entity was not found., Status: NOT_FOUND, Details: []"

func TestAGYNotFoundExecutorErrorInvalidatesConversation(t *testing.T) {
	if !AGYReplayedNotFound(agyNotFound) {
		t.Error("AGY NOT_FOUND executor error must invalidate the conversation")
	}
	for _, healthyError := range []string{
		"Error 404, Message: Requested entity was not found., Status: NOT_FOUND",
		"agent executor error: generating and executing: Error 503, Message: Service Unavailable",
		"agent executor error: generating and executing: Error 404, Message: Model \"claude-opus-5-5-high\" is not supported by any configured account in this group, Status: NOT_FOUND",
	} {
		if AGYReplayedNotFound(healthyError) {
			t.Errorf("healthy history discarded for %q", healthyError)
		}
	}
}

// TestAGYNotFoundIsNotAFreshRetryTrigger keeps the AGY 404 out of
// UnresumableHistory, which gates the daemon's in-turn fresh-session retry. A
// genuine current 404 must stay a failure (CHE-1710 current-404 control).
func TestAGYNotFoundIsNotAFreshRetryTrigger(t *testing.T) {
	if UnresumableHistory(agyNotFound) {
		t.Error("AGY NOT_FOUND must not open the fresh-session retry")
	}
}
