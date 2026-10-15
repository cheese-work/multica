package daemon

import (
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

const agyNotFound = "agent executor error: generating and executing: Error 404, Message: Requested entity was not found., Status: NOT_FOUND, Details: []"

func TestAGYNotFoundRetiresSession(t *testing.T) {
	reason, poisoned := classifyPoisonedError(agyNotFound)
	if !poisoned || reason != FailureReasonAPIInvalidRequest {
		t.Fatalf("classifyPoisonedError = (%q, %v), want (api_invalid_request, true)", reason, poisoned)
	}
}

func TestAGYNotFoundFreshRetryOnlyWithoutTools(t *testing.T) {
	result := agent.Result{Status: "failed", Error: agyNotFound, SessionID: "agy-session"}
	if !shouldRetryWithFreshSession(result, "agy-session", 0, "antigravity") {
		t.Error("resumed AGY turn with no tool use must retry on a fresh session")
	}
	if shouldRetryWithFreshSession(result, "agy-session", 3, "antigravity") {
		t.Error("AGY turn that used tools must not be re-run")
	}
}
