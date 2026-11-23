package browserlease

// AudienceSet is the token audience allowlist (plan r16, "Eligibility E"): the exact `aud`
// strings this deployment may sign. Membership is exact string equality, never a prefix match,
// and the zero value allows nothing, so a deployment with no allowlist signs nothing.
type AudienceSet map[string]struct{}

// NewAudienceSet builds a set from exact audience strings.
func NewAudienceSet(auds ...string) AudienceSet {
	s := make(AudienceSet, len(auds))
	for _, a := range auds {
		s[a] = struct{}{}
	}
	return s
}

// Has reports whether aud is a member.
func (s AudienceSet) Has(aud string) bool {
	_, ok := s[aud]
	return ok
}
