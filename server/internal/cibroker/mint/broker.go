package mint

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Minter creates and revokes installation tokens on the CI App.
type Minter interface {
	Mint(ctx context.Context, repoID int64, perms Permissions) (Token, error)
	Revoke(ctx context.Context, token Secret) error
}

// Token is a minted installation token.
type Token struct {
	Value       Secret
	ExpiresAt   time.Time
	Permissions Permissions
}

// Row is one audit row (rule 10). It has no field for a token or a JWT.
type Row struct {
	At                time.Time
	RepositoryID      int64
	Repository        string
	RunID             string
	RunAttempt        string
	WorkflowRef       string
	Event             string
	Ref               string
	RunnerEnvironment string
	Environment       string
	Permissions       Permissions
	Result            string
	Reason            string
}

// AuditStore persists audit rows.
type AuditStore interface {
	Record(ctx context.Context, row Row) error
}

// Request is a verified, policy-matched issuance request. The installation and
// the repository come from the caller's verified claims and server config.
type Request struct {
	RepositoryID      int64
	Repository        string
	RunID             string
	RunAttempt        string
	WorkflowRef       string
	Event             string
	Ref               string
	RunnerEnvironment string
	Environment       string
	Wanted            Permissions
	EntryMax          Permissions
}

// Broker issues tokens. One mutex serializes issuance (rule 6): the throttle,
// the quota reading and the mint of one request never interleave with another.
type Broker struct {
	sem      chan struct{}
	minter   Minter
	quota    *Quota
	throttle *Throttle
	audit    AuditStore
	ceiling  Permissions
	now      func() time.Time
	log      *slog.Logger
}

// Config wires a Broker. Ceiling is the phase 1 permission ceiling the broker
// applies on top of the CI App's own permission set.
type Config struct {
	Minter   Minter
	Quota    *Quota
	Throttle *Throttle
	Audit    AuditStore
	Ceiling  Permissions
	Now      func() time.Time
	Logger   *slog.Logger
}

// New builds a Broker.
func New(cfg Config) *Broker {
	b := &Broker{
		sem: make(chan struct{}, 1), minter: cfg.Minter, quota: cfg.Quota, throttle: cfg.Throttle,
		audit: cfg.Audit, ceiling: cfg.Ceiling, now: cfg.Now, log: cfg.Logger,
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.log == nil {
		b.log = slog.Default()
	}
	return b
}

const sideEffectTimeout = 5 * time.Second

// Issue serves one request. It returns a token, or a *Refusal (403 no usable
// permission, 429 throttle or quota cutoff, 503 quota unreadable, mint or audit
// failure). Every request that gets the lock writes one audit row; a request
// cancelled while it waits for the lock writes none. A token whose row cannot
// be written is revoked and refused, so no token exists without an audit row.
func (b *Broker) Issue(ctx context.Context, req Request) (Token, error) {
	select {
	case b.sem <- struct{}{}:
		defer func() { <-b.sem }()
	case <-ctx.Done():
		return Token{}, ctx.Err()
	}

	perms := Intersect(req.Wanted, req.EntryMax, b.ceiling)
	if len(perms) == 0 {
		return Token{}, b.refuse(ctx, req, perms, &Refusal{Status: http.StatusForbidden, Reason: "no_permissions"})
	}
	if reason := b.throttle.Check(req.RepositoryID, req.RunID, req.RunAttempt); reason != "" {
		return Token{}, b.refuse(ctx, req, perms, &Refusal{Status: http.StatusTooManyRequests, Reason: reason, RetryAfter: time.Minute})
	}
	if err := b.quota.Admit(ctx); err != nil {
		var ref *Refusal
		if !errors.As(err, &ref) {
			ref = &Refusal{Status: http.StatusServiceUnavailable, Reason: "quota_unavailable"}
		}
		return Token{}, b.refuse(ctx, req, perms, ref)
	}

	tok, err := b.minter.Mint(ctx, req.RepositoryID, perms)
	if err != nil {
		var up *UpstreamError
		if errors.As(err, &up) {
			b.log.Warn("ci broker mint failed", "op", up.Op, "status", up.Status)
		}
		return Token{}, b.refuse(ctx, req, perms, &Refusal{Status: http.StatusServiceUnavailable, Reason: "mint_failed", RetryAfter: 30 * time.Second})
	}

	if err := b.record(ctx, req, perms, "issued", ""); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sideEffectTimeout)
		defer cancel()
		if rerr := b.minter.Revoke(rctx, tok.Value); rerr != nil {
			b.log.Error("ci broker audit write failed and token revoke failed; a live token has no audit row",
				"repository_id", req.RepositoryID, "run_id", req.RunID, "revoke_error", rerr.Error())
		} else {
			b.log.Error("ci broker audit write failed; token revoked", "repository_id", req.RepositoryID, "run_id", req.RunID)
		}
		return Token{}, &Refusal{Status: http.StatusServiceUnavailable, Reason: "audit_unavailable", RetryAfter: 30 * time.Second}
	}
	b.throttle.Record(req.RepositoryID, req.RunID, req.RunAttempt)
	b.log.Info("ci broker issued token", "repository", req.Repository, "run_id", req.RunID, "run_attempt", req.RunAttempt, "permissions", perms)
	return tok, nil
}

func (b *Broker) refuse(ctx context.Context, req Request, perms Permissions, ref *Refusal) error {
	if err := b.record(ctx, req, perms, "refused", ref.Reason); err != nil {
		b.log.Error("ci broker audit write failed", "reason", ref.Reason, "repository_id", req.RepositoryID, "run_id", req.RunID)
	}
	b.log.Warn("ci broker refused", "repository", req.Repository, "run_id", req.RunID, "reason", ref.Reason, "status", ref.Status)
	return ref
}

func (b *Broker) record(ctx context.Context, req Request, perms Permissions, result, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sideEffectTimeout)
	defer cancel()
	return b.audit.Record(ctx, Row{
		At: b.now(), RepositoryID: req.RepositoryID, Repository: req.Repository,
		RunID: req.RunID, RunAttempt: req.RunAttempt, WorkflowRef: req.WorkflowRef,
		Event: req.Event, Ref: req.Ref, RunnerEnvironment: req.RunnerEnvironment,
		Environment: req.Environment, Permissions: perms, Result: result, Reason: reason,
	})
}
