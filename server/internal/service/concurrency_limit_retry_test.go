package service

import (
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// CHE-9849: the Codex account concurrency rejection used to retry within
// seconds (provider_network schedule: immediate, then 5s), so all three tries
// hit the still-saturated account. It now gets a growing backoff.
func TestConcurrencyLimitRetryBackoff(t *testing.T) {
	const net = "agent_error.provider_network"
	const raw = "Concurrency limit exceeded for account, please retry later"

	if got := retryAttemptCeiling(net, 2); got < 3 {
		t.Fatalf("ceiling = %d, want >= 3 tries", got)
	}

	first := retryDelayForFailure(net, raw, 1)
	second := retryDelayForFailure(net, raw, 2)
	if first < time.Minute {
		t.Errorf("delay after first failure = %s, want >= 1m (was immediate)", first)
	}
	if second <= first {
		t.Errorf("delay after second failure = %s, want growing past %s", second, first)
	}
	if second > gatewayFailureCooldown {
		t.Errorf("delay after second failure = %s, want <= %s cap", second, gatewayFailureCooldown)
	}

	// Every other failure keeps its existing schedule.
	const other = "API Error: Connection closed mid-response"
	if got := retryDelayForFailure(net, other, 1); got != 0 {
		t.Errorf("plain network cut, first failure: delay = %s, want 0", got)
	}
	if got := retryDelayForFailure(net, other, 2); got != providerNetworkFinalRetryWait {
		t.Errorf("plain network cut, second failure: delay = %s, want %s", got, providerNetworkFinalRetryWait)
	}
	if got := retryDelayForFailure(string(taskfailure.ReasonTimeout), raw, 1); got != 0 {
		t.Errorf("timeout reason with the same text: delay = %s, want 0", got)
	}
}
