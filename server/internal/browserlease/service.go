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
	Class3    *Class3Deny
	Now       func() time.Time
	// Audit records start and end events inside the transition's transaction (CHE-4744).
	Audit AuditFunc
	// Audiences is the token audience allowlist that E checks at grant, signing and renewal. The
	// zero value allows nothing: no grant and no token.
	Audiences AudienceSet

	// hook is a test seam that runs at named points inside the locked sections; nil in production.
	hook func(point string)
}

// at runs the test hook for point.
func (s *Service) at(point string) {
	if s.hook != nil {
		s.hook(point)
	}
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

// audit records a lease event through q, the transition's transaction.
func (s *Service) audit(ctx context.Context, q *db.Queries, kind string, l db.BrowserLease) error {
	if s.Audit == nil {
		return nil
	}
	return s.Audit(ctx, q, kind, l)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func isHookDenial(err error) bool {
	return errors.Is(err, ErrGateDenied) || errors.Is(err, ErrClassNotMintable) || errors.Is(err, ErrDecisionRequired) || errors.Is(err, ErrDecisionInvalid) ||
		errors.Is(err, ErrClass3Denied) || errors.Is(err, ErrClass3Unchecked)
}

// check applies the class rules, runs the user gate, the class-3 check for a login profile, then the
// decision for a class that needs one (S0 asks no one), and returns when the lease must end at the
// latest. Mint, renew and the queued grant all come through here, so a profile moved to a class that
// is not leased (H, S1) ends or drops its leases. leaseClass is the class the lease was granted
// under: a profile that changed class since (S0 to S2 or back) would otherwise keep the old host,
// token and decision rule, so any change ends or drops the lease.
func (s *Service) check(ctx context.Context, subj Subject, leaseClass string) (time.Time, error) {
	now := s.now()
	limit := now.Add(MaxLeaseDuration)
	// A member's login lease (no task) is class H only; an agent lease is never H.
	member := !subj.TaskID.Valid
	if member != (subj.Profile.Class == ClassH) || (!member && !mintable(subj.Profile.Class)) ||
		subj.Profile.Class != leaseClass {
		return limit, ErrClassNotMintable
	}
	if err := s.Gate.Check(ctx, subj); err != nil {
		return limit, err
	}
	if !needsDecision(subj.Profile.Class) {
		return limit, nil
	}
	// Class 3 is forbidden to every agent lease: no decision can allow it, so it is checked
	// before one is asked for, and again at every renew and queued grant. A member's own login
	// is not an agent lease.
	if !member {
		if err := s.Class3.checkClass3(subj.Profile.Hosts); err != nil {
			return limit, err
		}
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
	limit, err := s.check(ctx, subj, profile.Class)
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
	return s.signLease(ctx, res.Lease.ID, res.Lease.WorkspaceID, nil)
}

// Status reads a lease back without signing: no token, whatever the state.
func (s *Service) Status(ctx context.Context, ws, task, lease pgtype.UUID) (Result, error) {
	return s.statusOf(ctx, lease, ws, task, pgtype.UUID{}, false)
}

type signFailure struct{ err error }

func (e signFailure) Error() string { return fmt.Sprintf("sign browser token: %v", e.err) }
func (e signFailure) Unwrap() error { return e.err }

// signLease signs a token for an active lease in one transaction (plan r16, "Lock order": renew
// key, instance SHARE, credential SHARE, lease FOR UPDATE). E is checked on the rows it holds, the
// token is signed, its jti recorded, and the caller gets the token only after the commit, so a
// commit that fails leaves no token and no jti. limit is the renewal's revalidated deadline; the
// lease keeps the earlier of it and the stored one.
//
// A lease that has ended, or whose epoch is no longer the instance's, gets ErrLeaseEnded and is left
// alone: the instance may already belong to another lease. A lease that fails E is ended, outside
// the transaction (reason not_eligible), and the instance stays held (H). So is one whose signer
// fails or whose deadline passed.
func (s *Service) signLease(ctx context.Context, id, ws pgtype.UUID, limit *time.Time) (Result, error) {
	var out Result
	var held db.BrowserLease
	errExpired := errors.New("lease expired")
	err := s.inTx(ctx, func(q *db.Queries) error {
		s.at("sign:begin")
		if err := lockKey(ctx, q, "browser-lease-renew:"+uuid.UUID(id.Bytes).String()); err != nil {
			return err
		}
		// The instance id of an active lease never changes, so reading it before the locks is safe;
		// everything E depends on is read again under them.
		peek, err := q.GetBrowserLease(ctx, db.GetBrowserLeaseParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && peek.State != "active" {
			return ErrLeaseEnded
		}
		if err != nil {
			return err
		}
		in, err := q.LockBrowserInstanceShare(ctx, db.LockBrowserInstanceShareParams{ID: peek.InstanceID, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			held = peek
			return ErrNotEligible
		}
		if err != nil {
			return err
		}
		creds, err := q.LockBrowserGatewayCredentialsShare(ctx, db.LockBrowserGatewayCredentialsShareParams{WorkspaceID: ws, Gateways: []string{in.Gateway}})
		if err != nil {
			return err
		}
		l, err := q.LockBrowserLeaseForUpdate(ctx, db.LockBrowserLeaseForUpdateParams{ID: id, WorkspaceID: ws})
		if err != nil {
			return err
		}
		if l.State != "active" || !l.Epoch.Valid || l.Epoch.Int64 != in.Epoch {
			return ErrLeaseEnded
		}
		credentialed := map[string]bool{}
		for _, c := range creds {
			credentialed[c.Gateway] = true
		}
		host, err := eligible(l.Class, in, s.Audiences, credentialed)
		if err != nil {
			held = l
			return err
		}
		if limit != nil {
			// A decision shortened since the grant caps the lease from now on; the limit never grows.
			if l, err = q.ShortenBrowserLeaseExpiry(ctx, db.ShortenBrowserLeaseExpiryParams{ID: id, WorkspaceID: ws, ExpiresAt: ts(*limit)}); err != nil {
				return err
			}
		}
		if !s.now().Before(l.ExpiresAt.Time) {
			held = l
			return errExpired
		}
		s.at("sign:locked")
		res, err := s.token(ctx, q, l, host)
		if err != nil {
			held = l
			return err
		}
		s.at("sign:before-commit")
		out = res
		return nil
	})
	var sf signFailure
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, errExpired):
		return Result{}, errors.Join(ErrLeaseEnded, s.endActive(ctx, held, endExpired))
	case errors.Is(err, ErrNotEligible):
		return Result{}, errors.Join(err, s.endActive(ctx, held, endNotEligible))
	case errors.As(err, &sf):
		return Result{}, errors.Join(err, s.endActive(ctx, held, endSignFailed))
	}
	return Result{}, err
}

// token signs the claims for the locked lease row l on the host E settled and records the jti on
// the transaction q. The caller returns it to nobody until q commits.
func (s *Service) token(ctx context.Context, q *db.Queries, l db.BrowserLease, host string) (Result, error) {
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
		return Result{}, signFailure{err}
	}
	sum := sha256.Sum256([]byte(jti))
	hash := pgtype.Text{String: hex.EncodeToString(sum[:]), Valid: true}
	if err := q.SetBrowserLeaseJTI(ctx, db.SetBrowserLeaseJTIParams{ID: l.ID, WorkspaceID: l.WorkspaceID, JtiHash: hash}); err != nil {
		return Result{}, err
	}
	l.JtiHash = hash
	return Result{Lease: l, Host: host, Token: token, TokenExpires: exp}, nil
}

// endActive ends a granted lease. The instance stays held until cleanup-ack (W3-7).
func (s *Service) endActive(ctx context.Context, l db.BrowserLease, reason string) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		// Lock order: the instance row, then the lease row (signing and grant take them the same way).
		if _, err := q.LockBrowserInstance(ctx, db.LockBrowserInstanceParams{ID: l.InstanceID, WorkspaceID: l.WorkspaceID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		ended, err := q.EndBrowserLease(ctx, db.EndBrowserLeaseParams{
			ID: l.ID, WorkspaceID: l.WorkspaceID, EndedAt: ts(s.now()), EndReason: pgtype.Text{String: reason, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already ended
		}
		if err != nil {
			return err
		}
		if err := s.audit(ctx, q, AuditEnd, ended); err != nil {
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
	limit, err := s.check(ctx, subj, l.Class)
	if err != nil {
		if isHookDenial(err) {
			if endErr := s.endActive(ctx, l, endRevalidation); endErr != nil {
				return res, endErr
			}
		}
		return res, err
	}
	return s.signLease(ctx, l.ID, l.WorkspaceID, &limit)
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
		limit, err := s.check(ctx, subj, head.Class)
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
		s.at("grant:candidates")
		// The candidate list is advice. E is read from the rows as locked: every candidate instance
		// FOR UPDATE in id order, then the unrevoked credentials of their gateways FOR SHARE.
		locked, credentialed, err := s.lockForGrant(ctx, q, ws, candidates)
		if err != nil {
			return err
		}
		for _, c := range candidates {
			in, ok := locked[c.ID]
			if !ok {
				continue
			}
			if _, err := eligible(head.Class, in, s.Audiences, credentialed); err != nil {
				continue
			}
			s.at("grant:locked")
			claimed, err := q.ClaimIdleBrowserInstance(ctx, db.ClaimIdleBrowserInstanceParams{ID: c.ID, WorkspaceID: ws})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			granted, err := q.ActivateBrowserLease(ctx, db.ActivateBrowserLeaseParams{
				ID: head.ID, WorkspaceID: ws, InstanceID: claimed.ID, Epoch: pgtype.Int8{Int64: claimed.Epoch, Valid: true},
				GrantedAt: ts(s.now()), ExpiresAt: ts(limit),
			})
			if err != nil {
				return err
			}
			if err := s.audit(ctx, q, AuditStart, granted); err != nil {
				return err
			}
			more = true
			return nil
		}
		return nil
	})
	return more, err
}

// lockForGrant takes the grant's locks in the global order (plan r16, "Lock order"): the candidate
// instance rows in id order, then the unrevoked credential rows of their gateways in id order. It
// returns the instance rows as locked and the gateways that hold an unrevoked credential.
func (s *Service) lockForGrant(ctx context.Context, q *db.Queries, ws pgtype.UUID, candidates []Instance) (map[pgtype.UUID]db.BrowserInstance, map[string]bool, error) {
	ids := make([]pgtype.UUID, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	rows, err := q.LockBrowserInstancesForUpdate(ctx, db.LockBrowserInstancesForUpdateParams{WorkspaceID: ws, Ids: ids})
	if err != nil {
		return nil, nil, err
	}
	locked := make(map[pgtype.UUID]db.BrowserInstance, len(rows))
	gateways := make([]string, 0, len(rows))
	for _, in := range rows {
		locked[in.ID] = in
		gateways = append(gateways, in.Gateway)
	}
	creds, err := q.LockBrowserGatewayCredentialsShare(ctx, db.LockBrowserGatewayCredentialsShareParams{WorkspaceID: ws, Gateways: gateways})
	if err != nil {
		return nil, nil, err
	}
	credentialed := make(map[string]bool, len(creds))
	for _, c := range creds {
		credentialed[c.Gateway] = true
	}
	return locked, credentialed, nil
}
