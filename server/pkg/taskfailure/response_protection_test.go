package taskfailure

import "testing"

func TestResponseProtectionFailureIsNotNetworkRetryable(t *testing.T) {
	errText := "stream disconnected before completion: response protection is unavailable"
	if got := Classify(errText); got != ReasonAPIInvalidRequest {
		t.Errorf("Classify = %q, want %q (not a transient network retry)", got, ReasonAPIInvalidRequest)
	}
	if !UnresumableHistory(errText) {
		t.Error("response protection failure must invalidate the conversation")
	}
	for _, legacyReason := range []string{"agent_error", "agent_error.provider_network", "agent_error.unknown", ""} {
		if got := NormalizeDaemonReason(legacyReason, errText); got != ReasonAPIInvalidRequest {
			t.Errorf("NormalizeDaemonReason(%q) = %q, want %q", legacyReason, got, ReasonAPIInvalidRequest)
		}
	}
	for _, healthyError := range []string{"stream disconnected before completion", "connection closed mid-response", "response protection is available"} {
		if UnresumableHistory(healthyError) {
			t.Errorf("healthy history discarded for %q", healthyError)
		}
	}
}
