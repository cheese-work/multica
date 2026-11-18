package browserlease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	endExpired      = "expired"
	endReleased     = "released"
	endRevalidation = "revalidation_failed"
	endSignFailed   = "sign_failed"
)

// Service owns the lease queue. Gate, Decisions, Registry and Signer are the seams the later
// W3 issues fill.
type Service struct {
	Pool      *pgxpool.Pool
	Queries   *db.Queries
	Gate      Gate
	Decisions Decisions
	Registry  Registry
	Signer    Signer
	Now       func() time.Time
}

// MintRequest is a running task's ask for a lease. The caller resolves task, agent, issue and
// accountable user from the task row, never from the request body.
type MintRequest struct {
	WorkspaceID pgtype.UUID
	TaskID      pgtype.UUID
	AgentID     pgtype.UUID
	IssueID     pgtype.UUID
	UserID      pgtype.UUID
	ProfileID   pgtype.UUID
	DecisionID  pgtype.UUID
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func isHookDenial(err error) bool {
	return errors.Is(err, ErrGateDenied) || errors.Is(err, ErrDecisionRequired) || errors.Is(err, ErrDecisionInvalid)
}

// check runs the user gate, then the decision for a class that needs one (S0 asks no one), and
// returns when the lease must end at the latest.
func (s *Service) check(ctx context.Context, subj Subject) (time.Time, error) {
	now := s.now()
	limit := now.Add(MaxLeaseDuration)
	if err := s.Gate.Check(ctx, subj); err != nil {
		return limit, err
	}
	if subj.Profile.Class == ClassS0 {
		return limit, nil
	}
	exp, err := s.Decisions.Validate(ctx, subj)
	if err != nil {
		return limit, err
	}
	if exp != nil {
		if !exp.After(now) {
			return limit, ErrDecisionInvalid
		}
		if exp.Before(limit) {
			limit = *exp
		}
	}
	return limit, nil
}

func (s *Service) subjectOf(ctx context.Context, l db.BrowserLease) (Subject, error) {
	p, err := s.Queries.GetBrowserProfile(ctx, db.GetBrowserProfileParams{ID: l.ProfileID, WorkspaceID: l.WorkspaceID})
	if err != nil {
		return Subject{}, err
	}
	return Subject{
		WorkspaceID: l.WorkspaceID, UserID: l.UserID, AgentID: l.AgentID, IssueID: l.IssueID,
		TaskID: l.TaskID, Profile: p, DecisionID: l.DecisionID,
	}, nil
}

func lockKey(ctx context.Context, q *db.Queries, key string) error {
	return q.LockBrowserLeaseQueue(ctx, key)
}

func (s *Service) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Mint(ctx context.Context, r MintRequest) (Result, error) {
	// H is Cheese's login lease through the viewer (member auth, MintMember); S1 is deferred and
	// has no row.
	return s.mint(ctx, r, func(class string) bool { return class == ClassS0 || class == ClassS2 })
}

// MemberMintRequest is a member's ask for a class H login lease (CHE-5989). The caller is the
// authenticated member; there is no agent, task or issue.
type MemberMintRequest struct {
	WorkspaceID pgtype.UUID
	UserID      pgtype.UUID
	ProfileID   pgtype.UUID
	DecisionID  pgtype.UUID
}

// MintMember grants or queues a class H lease for a member. It runs the same gate, decision,
// registry, queue and token code as Mint; the registry serves class H on C00 only, so the token
// audience is the C00 host.
func (s *Service) MintMember(ctx context.Context, r MemberMintRequest) (Result, error) {
	return s.mint(ctx, MintRequest{
		WorkspaceID: r.WorkspaceID, UserID: r.UserID, ProfileID: r.ProfileID, DecisionID: r.DecisionID,
	}, func(class string) bool { return class == ClassH })
}

// mint is the shared mint path. mintable says which profile classes the caller may lease. A
// request without a task is a member's login lease and holds one open lease per member.
func (s *Service) mint(ctx context.Context, r MintRequest, mintable func(class string) bool) (Result, error) {
	if s.Signer == nil {
		return Result{}, ErrSignerMissing
	}
	profile, err := s.Queries.GetBrowserProfile(ctx, db.GetBrowserProfileParams{ID: r.ProfileID, WorkspaceID: r.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, err
	}
	if !mintable(profile.Class) {
		return Result{}, ErrClassNotMintable
	}
	subj := Subject{
		WorkspaceID: r.WorkspaceID, UserID: r.UserID, AgentID: r.AgentID, IssueID: r.IssueID,
		TaskID: r.TaskID, Profile: profile, DecisionID: r.DecisionID,
	}
	limit, err := s.check(ctx, subj)
	if err != nil {
		return Result{}, err
	}
	candidates, err := s.Registry.Instances(ctx, r.WorkspaceID, profile.Class)
	if err != nil {
		return Result{}, err
	}
	if len(candidates) == 0 {
		return Result{}, ErrNoInstance
	}

	var queued db.BrowserLease
	err = s.inTx(ctx, func(q *db.Queries) error {
		var err error
		if r.TaskID.Valid {
			if err := lockKey(ctx, q, "browser-lease-task:"+uuid.UUID(r.TaskID.Bytes).String()); err != nil {
				return err
			}
			_, err = q.GetOpenBrowserLeaseForTask(ctx, db.GetOpenBrowserLeaseForTaskParams{WorkspaceID: r.WorkspaceID, TaskID: r.TaskID})
		} else {
			if err := lockKey(ctx, q, "browser-lease-member:"+uuid.UUID(r.UserID.Bytes).String()); err != nil {
				return err
			}
			_, err = q.GetOpenBrowserLoginLeaseForUser(ctx, db.GetOpenBrowserLoginLeaseForUserParams{WorkspaceID: r.WorkspaceID, UserID: r.UserID})
		}
		if err == nil {
			return ErrLeaseExists
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// The instance is a placeholder until the grant picks one.
		queued, err = q.CreateQueuedBrowserLease(ctx, db.CreateQueuedBrowserLeaseParams{
			WorkspaceID: r.WorkspaceID, InstanceID: candidates[0].ID, ProfileID: r.ProfileID,
			AgentID: r.AgentID, TaskID: r.TaskID, IssueID: r.IssueID, UserID: r.UserID,
			DecisionID: r.DecisionID, Class: profile.Class, ExpiresAt: ts(limit),
		})
		return err
	})
	if err != nil {
		return Result{}, err
	}
	if err := s.AdvanceQueue(ctx, r.WorkspaceID, profile.Class); err != nil {
		return Result{}, err
	}
	return s.statusOf(ctx, queued.ID, r.WorkspaceID, r.TaskID, r.UserID, true)
}

// statusOf reads the lease back; a granted lease gets its token only when issueToken is set. An
// agent lease is looked up by its task, a member's login lease by its member.
func (s *Service) statusOf(ctx context.Context, id, ws, task, user pgtype.UUID, issueToken bool) (Result, error) {
	var l db.BrowserLease
	var err error
	if task.Valid {
		l, err = s.Queries.GetBrowserLeaseForTask(ctx, db.GetBrowserLeaseForTaskParams{ID: id, WorkspaceID: ws, TaskID: task})
	} else {
		l, err = s.Queries.GetBrowserLoginLeaseForUser(ctx, db.GetBrowserLoginLeaseForUserParams{ID: id, WorkspaceID: ws, UserID: user})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// A queued lease that failed revalidation at its grant leaves the queue.
		return Result{}, ErrGateDenied
	}
	if err != nil {
		return Result{}, err
	}
	res := Result{Lease: l}
	switch l.State {
	case "queued":
		res.QueuePosition, err = s.Queries.CountBrowserLeasesAhead(ctx, l.ID)
		return res, err
	case "ended":
		return res, ErrLeaseEnded
	}
	if !issueToken {
		return res, nil
	}
	return s.issueTokenEnding(ctx, res)
}

// issueToken signs with the lease row it is given, so the caller must pass the persisted
// deadline. A signer failure comes back as signFailure; the caller ends the lease outside any
// transaction that already holds the lease row.
func (s *Service) issueToken(ctx context.Context, q *db.Queries, res Result) (Result, error) {
	l := res.Lease
	instances, err := s.Registry.Instances(ctx, l.WorkspaceID, l.Class)
	if err != nil {
		return res, err
	}
	var host string
	for _, in := range instances {
		if in.ID == l.InstanceID {
			host = in.Host
		}
	}
	if host == "" {
		return res, ErrNoInstance
	}
	now := s.now()
	exp := now.Add(TokenTTL)
	if l.ExpiresAt.Time.Before(exp) {
		exp = l.ExpiresAt.Time
	}
	jti := uuid.NewString()
	claims := Claims{
		Aud: Aud(host), Sub: uuid.UUID(l.AgentID.Bytes).String(), LeaseID: uuid.UUID(l.ID.Bytes).String(),
		Epoch: l.Epoch.Int64, TaskID: uuid.UUID(l.TaskID.Bytes).String(), IssueID: uuid.UUID(l.IssueID.Bytes).String(),
		UserID: uuid.UUID(l.UserID.Bytes).String(), Profile: uuid.UUID(l.ProfileID.Bytes).String(),
		Class: l.Class, Instance: uuid.UUID(l.InstanceID.Bytes).String(), Iat: now.Unix(), Exp: exp.Unix(), JTI: jti,
	}
	if !l.TaskID.Valid {
		// A member's login lease: the member is the subject, and there is no task or issue.
		claims.Sub, claims.TaskID, claims.IssueID = claims.UserID, "", ""
	}
	if l.DecisionID.Valid {
		claims.DecisionID = uuid.UUID(l.DecisionID.Bytes).String()
	}
	token, err := s.Signer.Sign(claims)
	if err != nil {
		return res, signFailure{err}
	}
	sum := sha256.Sum256([]byte(jti))
	if err := q.SetBrowserLeaseJTI(ctx, db.SetBrowserLeaseJTIParams{
		ID: l.ID, WorkspaceID: l.WorkspaceID, JtiHash: pgtype.Text{String: hex.EncodeToString(sum[:]), Valid: true},
	}); err != nil {
		return res, err
	}
	res.Lease.JtiHash = pgtype.Text{String: hex.EncodeToString(sum[:]), Valid: true}
	res.Host, res.Token, res.TokenExpires = host, token, exp
	return res, nil
}

type signFailure struct{ err error }

func (e signFailure) Error() string { return fmt.Sprintf("sign browser token: %v", e.err) }
func (e signFailure) Unwrap() error { return e.err }

// issueTokenEnding is issueToken for a caller outside any transaction: a signer failure ends
// the lease.
func (s *Service) issueTokenEnding(ctx context.Context, res Result) (Result, error) {
	out, err := s.issueToken(ctx, s.Queries, res)
	var sf signFailure
	if errors.As(err, &sf) {
		return out, errors.Join(err, s.endActive(ctx, res.Lease, endSignFailed))
	}
	return out, err
}

// endActive ends a granted lease. The instance stays held until cleanup-ack (W3-7).
func (s *Service) endActive(ctx context.Context, l db.BrowserLease, reason string) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		ended, err := q.EndBrowserLease(ctx, db.EndBrowserLeaseParams{
			ID: l.ID, WorkspaceID: l.WorkspaceID, EndedAt: ts(s.now()), EndReason: pgtype.Text{String: reason, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already ended
		}
		if err != nil {
			return err
		}
		return q.SetBrowserInstancePhase(ctx, db.SetBrowserInstancePhaseParams{
			ID: ended.InstanceID, WorkspaceID: ended.WorkspaceID, Phase: "draining",
		})
	})
}

func (s *Service) Renew(ctx context.Context, ws, task, lease pgtype.UUID) (Result, error) {
	return s.renew(ctx, lease, ws, task, pgtype.UUID{})
}

// RenewMember renews a member's own class H lease.
func (s *Service) RenewMember(ctx context.Context, ws, user, lease pgtype.UUID) (Result, error) {
	return s.renew(ctx, lease, ws, pgtype.UUID{}, user)
}

func (s *Service) renew(ctx context.Context, lease, ws, task, user pgtype.UUID) (Result, error) {
	if s.Signer == nil {
		return Result{}, ErrSignerMissing
	}
	res, err := s.statusOf(ctx, lease, ws, task, user, false)
	if errors.Is(err, ErrGateDenied) {
		return Result{}, ErrNotFound // statusOf reports a missing row as a dropped lease
	}
	if err != nil || res.Lease.State == "queued" {
		return res, err
	}
	l := res.Lease
	if !s.now().Before(l.ExpiresAt.Time) {
		if err := s.endActive(ctx, l, endExpired); err != nil {
			return res, err
		}
		return res, ErrLeaseEnded
	}
	subj, err := s.subjectOf(ctx, l)
	if err != nil {
		return res, err
	}
	limit, err := s.check(ctx, subj)
	if err != nil {
		if isHookDenial(err) {
			if endErr := s.endActive(ctx, l, endRevalidation); endErr != nil {
				return res, endErr
			}
		}
		return res, err
	}
	return s.renewToken(ctx, l, limit)
}

// renewToken persists the revalidated deadline and signs under one per-lease lock. Overlapping
// renewals take turns: each one reads the persisted minimum back from its own UPDATE, so the
// last token signed and recorded never outlives a deadline another renewal already shortened.
func (s *Service) renewToken(ctx context.Context, l db.BrowserLease, limit time.Time) (Result, error) {
	var res Result
	errExpired := errors.New("lease expired")
	err := s.inTx(ctx, func(q *db.Queries) error {
		if err := lockKey(ctx, q, "browser-lease-renew:"+uuid.UUID(l.ID.Bytes).String()); err != nil {
			return err
		}
		// A decision shortened since the grant caps the lease from now on; the limit never grows.
		cur, err := q.ShortenBrowserLeaseExpiry(ctx, db.ShortenBrowserLeaseExpiryParams{
			ID: l.ID, WorkspaceID: l.WorkspaceID, ExpiresAt: ts(limit),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseEnded // released or ended while this renewal was validating
		}
		if err != nil {
			return err
		}
		if !s.now().Before(cur.ExpiresAt.Time) {
			return errExpired
		}
		res, err = s.issueToken(ctx, q, Result{Lease: cur})
		return err
	})
	var sf signFailure
	switch {
	case errors.Is(err, errExpired):
		if endErr := s.endActive(ctx, l, endExpired); endErr != nil {
			return res, endErr
		}
		return res, ErrLeaseEnded
	case errors.As(err, &sf):
		return res, errors.Join(err, s.endActive(ctx, l, endSignFailed))
	}
	return res, err
}

func (s *Service) Release(ctx context.Context, ws, task, lease pgtype.UUID) error {
	l, err := s.Queries.GetBrowserLeaseForTask(ctx, db.GetBrowserLeaseForTaskParams{ID: lease, WorkspaceID: ws, TaskID: task})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	switch l.State {
	case "queued":
		_, err = s.Queries.DeleteQueuedBrowserLease(ctx, db.DeleteQueuedBrowserLeaseParams{ID: l.ID, WorkspaceID: ws})
		return err
	case "active":
		return s.endActive(ctx, l, endReleased)
	}
	return nil
}

// AdvanceQueue grants the head of a class queue while an instance is idle. A head that fails
// revalidation leaves the queue and the next one is tried. W3-7 calls it after cleanup-ack.
func (s *Service) AdvanceQueue(ctx context.Context, ws pgtype.UUID, class string) error {
	for {
		more, err := s.advanceOne(ctx, ws, class)
		if err != nil || !more {
			return err
		}
	}
}

// advanceOne handles the head of the queue; more reports that the queue may hold another
// grantable lease.
func (s *Service) advanceOne(ctx context.Context, ws pgtype.UUID, class string) (more bool, err error) {
	err = s.inTx(ctx, func(q *db.Queries) error {
		if err := lockKey(ctx, q, "browser-lease-queue:"+uuid.UUID(ws.Bytes).String()+":"+class); err != nil {
			return err
		}
		head, err := q.HeadQueuedBrowserLease(ctx, db.HeadQueuedBrowserLeaseParams{WorkspaceID: ws, Class: class})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		subj, err := s.subjectOf(ctx, head)
		if err != nil {
			return err
		}
		limit, err := s.check(ctx, subj)
		if isHookDenial(err) {
			// The lease never held an instance and has no epoch, so it cannot be 'ended'.
			_, err = q.DeleteQueuedBrowserLease(ctx, db.DeleteQueuedBrowserLeaseParams{ID: head.ID, WorkspaceID: ws})
			more = err == nil
			return err
		}
		if err != nil {
			return err
		}
		candidates, err := s.Registry.Instances(ctx, ws, class)
		if err != nil {
			return err
		}
		for _, c := range candidates {
			claimed, err := q.ClaimIdleBrowserInstance(ctx, db.ClaimIdleBrowserInstanceParams{ID: c.ID, WorkspaceID: ws})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			_, err = q.ActivateBrowserLease(ctx, db.ActivateBrowserLeaseParams{
				ID: head.ID, WorkspaceID: ws, InstanceID: claimed.ID, Epoch: pgtype.Int8{Int64: claimed.Epoch, Valid: true},
				GrantedAt: ts(s.now()), ExpiresAt: ts(limit),
			})
			more = err == nil
			return err
		}
		return nil
	})
	return more, err
}
