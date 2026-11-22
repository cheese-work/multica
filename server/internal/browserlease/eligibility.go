package browserlease

import "errors"

// ErrNotEligible means E (plan r16, "Eligibility E") failed for the lease and instance: no grant
// and no token.
var ErrNotEligible = errors.New("browserlease: lease is not eligible for the instance")

// endNotEligible is the end reason of an active lease that failed E at signing.
const endNotEligible = "not_eligible"
