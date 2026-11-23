package browserlease

import (
	"errors"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrNotEligible means E (plan r16, "Eligibility E") failed for the lease and instance: no grant
// and no token.
var ErrNotEligible = errors.New("browserlease: lease is not eligible for the instance")

// endNotEligible is the end reason of an active lease that failed E at signing.
const endNotEligible = "not_eligible"

// eligible is E for a lease class and the instance row, read under the locks of the lock order
// (instance, then credential): the row passes the parity reader, serves the class, the host
// audience is on the allowlist, and an unrevoked credential of the instance's gateway exists.
// credentialed names the gateways that hold one. It returns the host the token audience comes
// from. The lease-side checks (active, epoch equals the instance epoch) belong to the signing
// transaction, which holds the lease row. Readiness R arrives with A1e.
func eligible(class string, in db.BrowserInstance, allow AudienceSet, credentialed map[string]bool) (string, error) {
	host, ok := parityHost(in)
	if !ok || !hostServes(host, class) || !allow.Has(Aud(host)) || !credentialed[in.Gateway] {
		return "", ErrNotEligible
	}
	return host, nil
}
