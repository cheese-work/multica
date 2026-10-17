package taskfailure

import "testing"

// AGY stores its malformed-function-call retry notice in the conversation and
// reports it as the error of every later resumed turn, even when that turn
// completes with a DONE answer (CHE-1812).
const agyMalformedCall = "Your previous response contained an improperly formatted function call\nPlease retry with a properly formatted function call\nRetries remaining: 3"

func TestAGYMalformedCallNoticeInvalidatesConversation(t *testing.T) {
	if !AGYReplayedNotFound(agyMalformedCall) {
		t.Error("AGY malformed-function-call retry notice must invalidate the conversation")
	}
	for _, healthyError := range []string{
		"Please retry with a properly formatted request",
		"function call failed: tool timed out",
		"Retries remaining: 3",
	} {
		if AGYReplayedNotFound(healthyError) {
			t.Errorf("healthy history discarded for %q", healthyError)
		}
	}
}

// Like the NOT_FOUND replay, the notice must stay out of UnresumableHistory:
// that predicate opens the in-turn fresh-session retry (CHE-1710 semantics).
func TestAGYMalformedCallIsNotAFreshRetryTrigger(t *testing.T) {
	if UnresumableHistory(agyMalformedCall) {
		t.Error("AGY malformed-call notice must not open the fresh-session retry")
	}
}
