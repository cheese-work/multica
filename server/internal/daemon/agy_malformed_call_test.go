package daemon

import (
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

const agyMalformedCall = "Your previous response contained an improperly formatted function call\nPlease retry with a properly formatted function call\nRetries remaining: 3"

func TestAGYMalformedCallRetiresSession(t *testing.T) {
	reason, poisoned := classifyPoisonedError(agyMalformedCall)
	if !poisoned || reason != FailureReasonAPIInvalidRequest {
		t.Fatalf("classifyPoisonedError = (%q, %v), want (api_invalid_request, true)", reason, poisoned)
	}
}

// The turn fails once and the next turn starts fresh: no in-turn fresh retry on
// any branch, with or without a SessionID.
func TestAGYMalformedCallNeverRetriesFresh(t *testing.T) {
	for _, provider := range agent.SupportedTypes {
		for _, sessionID := range []string{"agy-session", ""} {
			for _, rejected := range []bool{false, true} {
				result := agent.Result{Status: "failed", Error: agyMalformedCall, SessionID: sessionID, ResumeRejected: rejected}
				if shouldRetryWithFreshSession(result, "agy-session", 0, provider) {
					t.Errorf("%s (session %q, rejected %v): AGY malformed-call notice must never take the fresh-session retry", provider, sessionID, rejected)
				}
			}
		}
	}
}
