// Package cibroker verifies GitHub Actions OIDC JWTs and matches them against a
// per-workflow policy (CHE-4257 plan r10, rules 2-4). Slice S1: verification and
// policy only. Token issuance, quota and throttle (S2) are not implemented here.
package cibroker

import (
	"context"
	"fmt"
	"time"
)

const (
	Issuer   = "https://token.actions.githubusercontent.com"
	JWKSURL  = Issuer + "/.well-known/jwks"
	Audience = "multica-ci-broker"
	MainRef  = "refs/heads/main"

	maxIAT = 5 * time.Minute // reject an older iat
	skew   = 60 * time.Second
)

// Refusal is a request the broker rejects. Status is the HTTP status.
type Refusal struct {
	Status int
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(status int, format string, a ...any) *Refusal {
	return &Refusal{Status: status, Reason: fmt.Sprintf(format, a...)}
}

// Claims are the verified claims the policy needs. GitHub sends every id as a string.
type Claims struct {
	Repository, RepositoryID, RepositoryOwnerID string
	Ref, EventName, WorkflowRef, JobWorkflowRef string
	RunnerEnvironment, Environment, JTI         string
	RunID, RunAttempt                           string
	Expires, IssuedAt                           time.Time
}

// Broker authorizes a JWT: verify it (rule 2), then match the policy (rules 3-4).
type Broker struct {
	Verifier *Verifier
	Policy   *Policy
}

// Authorize returns the matching policy entry. The error is always a *Refusal.
func (b *Broker) Authorize(ctx context.Context, token string) (*Entry, *Claims, error) {
	c, err := b.Verifier.Verify(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	e, err := b.Policy.Match(c)
	if err != nil {
		return nil, nil, err
	}
	return e, c, nil
}
