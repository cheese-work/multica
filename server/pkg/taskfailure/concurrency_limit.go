package taskfailure

import "strings"

// concurrencyLimitWitness is the Codex account rejection ("Concurrency limit
// exceeded for account, please retry later"). It stays in provider_network:
// that reason retries up to three times and resumes the session, and it sits
// outside the held provider path, which allows one recovery per lineage.
const concurrencyLimitWitness = "concurrency limit exceeded"

// IsConcurrencyLimit reports whether a failure text is the account-level
// concurrency rejection, so the retry scheduler can back it off instead of
// firing back into a still-saturated account (CHE-9849).
func IsConcurrencyLimit(rawError string) bool {
	return strings.Contains(strings.ToLower(rawError), concurrencyLimitWitness)
}
