// Package mint is slice S2 of the CI token broker (CHE-4257 rules 5-7, 9-10):
// it turns an already-verified, already-policy-matched request into a GitHub
// installation token from the dedicated CI App.
//
// The caller (S1) verifies the OIDC JWT and picks the policy entry. This
// package never sees the JWT. It intersects permissions, applies the
// best-effort quota cutoff and the throttle, mints, writes the audit row and
// returns the token. No token and no JWT is ever logged or audited.
package mint
