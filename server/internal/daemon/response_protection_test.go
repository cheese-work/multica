package daemon

import "testing"

func TestResponseProtectionRetiresSession(t *testing.T) {
	reason, poisoned := classifyPoisonedError("stream disconnected before completion: response protection is unavailable")
	if !poisoned || reason != FailureReasonAPIInvalidRequest {
		t.Fatalf("classifyPoisonedError = (%q, %v), want (api_invalid_request, true)", reason, poisoned)
	}
}
