// Package browserlease mints, renews, releases and queues browser leases (CHE-4732, W3-3; plan
// CHE-4695 W1 r5, "Token model" and "Permissions, session assignment").
package browserlease

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	ClassS0 = "S0"
	ClassS2 = "S2"
	ClassH  = "H"

	// TokenTTL is the life of one token; a running task renews it.
	TokenTTL = 15 * time.Minute
	// MaxLeaseDuration is the hard limit of a lease, or the decision's duration if shorter.
	MaxLeaseDuration = 2 * time.Hour

	HostX99 = "x99"
	HostC00 = "c00"
)

// mintable is the class rule for an agent lease (W3-12, D3 = A). S0 and S2 are leased. H is
// Cheese's own login through the viewer, never an agent lease. S1 is deferred and rejected, as is
// any other value.
func mintable(class string) bool { return class == ClassS0 || class == ClassS2 }

// needsDecision: S0 asks no one (user gate only); every other class needs a decision per lease.
func needsDecision(class string) bool { return class != ClassS0 }

var (
	ErrClassNotMintable = errors.New("class cannot be leased to an agent")
	ErrGateDenied       = errors.New("user or agent has no browser permission for the profile")
	ErrDecisionRequired = errors.New("a decision is required")
	ErrDecisionInvalid  = errors.New("the decision does not cover this lease")
	ErrLeaseExists      = errors.New("the task already holds an open lease")
	ErrNoInstance       = errors.New("no instance serves this class")
	ErrNotFound         = errors.New("lease not found")
	ErrLeaseEnded       = errors.New("lease has ended")
	ErrSignerMissing    = errors.New("token signing is not configured")
	ErrNotImplemented   = errors.New("not implemented")
)

// DecisionRequiredError is ErrDecisionRequired plus the request that asks for the decision
// (W3-10). errors.Is(err, ErrDecisionRequired) holds.
type DecisionRequiredError struct{ RequestID pgtype.UUID }

func (DecisionRequiredError) Error() string        { return ErrDecisionRequired.Error() }
func (DecisionRequiredError) Is(target error) bool { return target == ErrDecisionRequired }

// Claims is the token payload (plan "Token model"). Signing and the JWKS route are W3-4.
type Claims struct {
	Aud        string `json:"aud"`
	Sub        string `json:"sub"`
	LeaseID    string `json:"lease_id"`
	Epoch      int64  `json:"epoch"`
	TaskID     string `json:"task_id"`
	IssueID    string `json:"issue_id"`
	DecisionID string `json:"decision_id,omitempty"`
	UserID     string `json:"user_id"`
	Profile    string `json:"profile"`
	Class      string `json:"class"`
	Instance   string `json:"instance"`
	Iat        int64  `json:"iat"`
	Exp        int64  `json:"exp"`
	JTI        string `json:"jti"`
}

// Signer produces the compact JWS for Claims. W3-4 supplies the EdDSA signer.
type Signer interface {
	Sign(Claims) (string, error)
}

// Subject is what the gate and decision hooks judge.
type Subject struct {
	WorkspaceID pgtype.UUID
	UserID      pgtype.UUID
	AgentID     pgtype.UUID
	IssueID     pgtype.UUID
	TaskID      pgtype.UUID
	Profile     db.BrowserProfile
	DecisionID  pgtype.UUID // invalid when the caller named none
}

// Gate is the user gate hook (W3-8 model, W3-9 enforcement): the task's accountable user holds
// browser permission for the profile and the agent is on its allowlist. Return ErrGateDenied.
type Gate interface {
	Check(ctx context.Context, s Subject) error
}

// Decisions is the decision hook (W3-10, W3-11). It returns ErrDecisionRequired or
// ErrDecisionInvalid, and the decision's own expiry when it has one.
type Decisions interface {
	Validate(ctx context.Context, s Subject) (expiresAt *time.Time, err error)
}

// DenyAll is the default until W3-8 to W3-11 land: default deny.
type DenyAll struct{}

func (DenyAll) Check(context.Context, Subject) error { return ErrGateDenied }
func (DenyAll) Validate(context.Context, Subject) (*time.Time, error) {
	return nil, ErrDecisionRequired
}

// Instance is one registry entry. Host decides the token audience and the classes served.
type Instance struct {
	db.BrowserInstance
	Host string
}

// Registry is the instance registry (W3-18). Instances returns those serving the class, in the
// order the queue tries them.
type Registry interface {
	Instances(ctx context.Context, workspaceID pgtype.UUID, class string) ([]Instance, error)
}

// Aud is the token audience for an instance host.
func Aud(host string) string { return "steel-gateway/" + host }

// Result is the outcome of mint or renew. Token is empty while the lease is queued.
type Result struct {
	Lease         db.BrowserLease
	Host          string
	Token         string
	TokenExpires  time.Time
	QueuePosition int64 // 1 is next; 0 when not queued
}
