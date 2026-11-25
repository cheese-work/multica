package taskfailure

import "testing"

// The Codex account concurrency rejection is transient and shares the
// provider_network reason (its retry is resume-safe); IsConcurrencyLimit is the
// text witness the retry scheduler uses to back it off (CHE-9849).
func TestIsConcurrencyLimit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"codex wording", "Concurrency limit exceeded for account, please retry later", true},
		{"wrapped, mixed case", "stream error: 429 CONCURRENCY LIMIT EXCEEDED for account", true},
		{"anthropic wording is a different bucket", "You've reached your concurrent request limit.", false},
		{"plain disconnect", "API Error: Connection closed mid-response", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := IsConcurrencyLimit(tc.raw); got != tc.want {
			t.Errorf("%s: IsConcurrencyLimit(%q) = %v, want %v", tc.name, tc.raw, got, tc.want)
		}
	}
	// The daemon labels this failure provider_network; normalising it with the
	// text must keep that reason (the retry is resume-safe and sits outside the
	// one-retry held provider path).
	const raw = "Concurrency limit exceeded for account, please retry later"
	if got := NormalizeDaemonReason("agent_error.provider_network", raw); got != ReasonAgentProviderNetwork {
		t.Errorf("NormalizeDaemonReason = %s, want %s", got, ReasonAgentProviderNetwork)
	}
}
