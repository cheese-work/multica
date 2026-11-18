package browserlease

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Once-per-issue S2 grant (plan W1 r5, "Permissions, session assignment, decision gate and D3";
// CHE-4782, W3-19). One answered decision for "once per issue for profile P" covers every lease
// of P on that issue by the named agent until the issue reaches a done or closed status, the
// decision's duration ends, or GrantMaxAge passes, whichever comes first. Each lease still ends
// at MaxLeaseDuration and is revalidated at every mint, renew and queued grant, because
// Service.check calls Decisions.Validate each time. Withdrawal revokes through
// Revoker.RevokeDecision.

// GrantMaxAge is the cap on a once-per-issue grant. It is a reversible parameter: Cheese can
// veto the 7 days.
const GrantMaxAge = 7 * 24 * time.Hour

// Grant is an answered once-per-issue decision, as the decision store (W3-11) reads it back.
type Grant struct {
	DecisionID  pgtype.UUID
	WorkspaceID pgtype.UUID
	IssueID     pgtype.UUID
	AgentID     pgtype.UUID
	ProfileID   pgtype.UUID
	AnsweredAt  time.Time
	EndsAt      *time.Time // the decision's own duration end, nil when it names none
	Withdrawn   bool
}

// GrantSource finds a once-per-issue grant by decision id. ok is false for any other decision.
type GrantSource interface {
	Grant(ctx context.Context, ws, decision pgtype.UUID) (g Grant, ok bool, err error)
}

// IssueState says whether an issue reached a done or closed status.
type IssueState interface {
	Closed(ctx context.Context, ws, issue pgtype.UUID) (bool, error)
}

// OncePerIssue is a Decisions hook. A decision that is a once-per-issue grant is judged here;
// any other decision goes to Next (the per-lease validation).
type OncePerIssue struct {
	Grants GrantSource
	Issues IssueState
	Next   Decisions
	Now    func() time.Time
}

func (o OncePerIssue) Validate(ctx context.Context, s Subject) (*time.Time, error) {
	if !s.DecisionID.Valid {
		return o.Next.Validate(ctx, s)
	}
	g, ok, err := o.Grants.Grant(ctx, s.WorkspaceID, s.DecisionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return o.Next.Validate(ctx, s)
	}
	now := time.Now()
	if o.Now != nil {
		now = o.Now()
	}
	if g.Withdrawn || g.WorkspaceID != s.WorkspaceID || g.IssueID != s.IssueID ||
		g.AgentID != s.AgentID || g.ProfileID != s.Profile.ID || s.Profile.Class != ClassS2 {
		return nil, ErrDecisionInvalid
	}
	end := g.AnsweredAt.Add(GrantMaxAge)
	if g.EndsAt != nil && g.EndsAt.Before(end) {
		end = *g.EndsAt
	}
	if !end.After(now) {
		return nil, ErrDecisionInvalid
	}
	closed, err := o.Issues.Closed(ctx, s.WorkspaceID, s.IssueID)
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrDecisionInvalid
	}
	return &end, nil
}

// QueryIssueState reads the issue row. A built-in done or cancelled status closes it, and so
// does a custom status in a done, closed or cancelled category. A missing issue counts as
// closed: the grant covers nothing.
type QueryIssueState struct{ Queries *db.Queries }

func (q QueryIssueState) Closed(ctx context.Context, ws, issue pgtype.UUID) (bool, error) {
	closed, err := q.Queries.IsBrowserGrantIssueClosed(ctx, db.IsBrowserGrantIssueClosedParams{ID: issue, WorkspaceID: ws})
	if err != nil {
		return false, errors.Join(errors.New("read issue status for browser grant"), err)
	}
	return closed, nil
}
