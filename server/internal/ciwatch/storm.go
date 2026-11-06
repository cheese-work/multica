package ciwatch

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// CHE-4279 (S7): P2/P3/P4 and incident executors with the storm breaker.

const (
	ReasonBreaker          = "breaker"
	ReasonCap              = "cap"
	ReasonNonTransactional = "non_transactional_destination"
	ReasonWakeCapped       = "wake_capped"
	StormThreshold         = 5
	StormWindow            = 10 * time.Minute
	StormPause             = 30 * time.Minute
	DigestEvery            = 6 * time.Hour
	ReopenAfter            = 24 * time.Hour
	ReopenAfterIncident    = 7 * 24 * time.Hour
)

// Issue kinds a signature owner carries.
const (
	KindP2       = "p2"
	KindP3       = "p3"
	KindIncident = "incident"
)

// Admit outcomes.
const (
	OutcomeScheduled  = "scheduled"  // this run owns the issue creation
	OutcomeAttached   = "attached"   // counted on the signature's owner
	OutcomeSuppressed = "suppressed" // P5: breaker tripped or paused
	OutcomeNotify     = "notify"     // P4 notification scheduled
	OutcomeDuplicate  = "duplicate"  // this run was admitted before
)

// Failure is one classified failed run handed to Admit.
type Failure struct {
	WorkspaceID, RepoOwner, RepoName string
	RunID                            int64
	SignatureHash                    string
	Protocol                         string // "P2", "P3" or "P4"
	RunURL                           string
}

type Admission struct{ Outcome string }

// Admit runs classification's follow-up in one transaction under the advisory
// lock on (repo, signature): breaker check, owner claim, action scheduling.
func (l *Ledger) Admit(ctx context.Context, f Failure) (Admission, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return Admission{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := l.admit(ctx, tx, f)
	if err != nil {
		return Admission{}, err
	}
	return out, tx.Commit(ctx)
}

func (l *Ledger) admit(ctx context.Context, tx pgx.Tx, f Failure) (Admission, error) {
	now := l.now()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		ownerKey(f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash)); err != nil {
		return Admission{}, err
	}
	var seen bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ci_watch_event WHERE workspace_id=$1::uuid AND repo_owner=$2
		AND repo_name=$3 AND signature_hash=$4 AND kind='admit' AND run_id=$5)`,
		f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, f.RunID).Scan(&seen); err != nil {
		return Admission{}, err
	}
	if seen {
		return Admission{Outcome: OutcomeDuplicate}, nil
	}
	if err := l.recordEvent(ctx, tx, f, "admit", nil, ""); err != nil {
		return Admission{}, err
	}
	if f.Protocol == "P4" {
		// A notification is already deduplicated per (repo, signature, day); no owner, no breaker.
		_, _, err := l.schedule(ctx, tx, l.newAction(f, ActionInboxNotify, "p4"))
		return Admission{Outcome: OutcomeNotify}, err
	}

	kind := KindP2
	if f.Protocol == "P3" {
		kind = KindP3
	}
	var pausedUntil *time.Time
	var recent int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT max(paused_until) FROM ci_watch_event WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3
			AND signature_hash=$4 AND kind='breaker'),
		(SELECT count(*) FROM ci_watch_event WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3
			AND signature_hash=$4 AND kind='admit' AND created_at > $5)`,
		f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, now.Add(-StormWindow)).Scan(&pausedUntil, &recent); err != nil {
		return Admission{}, err
	}
	paused := pausedUntil != nil && pausedUntil.After(now)

	switch {
	case !paused && recent >= StormThreshold:
		until := now.Add(StormPause)
		if err := l.recordEvent(ctx, tx, f, "breaker", &until, ""); err != nil {
			return Admission{}, err
		}
		if _, err := l.cancelScheduled(ctx, tx, CancelFilter{WorkspaceID: f.WorkspaceID, RepoOwner: f.RepoOwner,
			RepoName: f.RepoName, SignatureHash: f.SignatureHash}, ReasonBreaker, true); err != nil {
			return Admission{}, err
		}
		if err := l.recordEvent(ctx, tx, f, "suppressed", nil, "breaker_trip"); err != nil {
			return Admission{}, err
		}
		// One incident through the signature owner. The count already includes
		// runs that attached to a creating owner the cancel just released.
		return l.attach(ctx, tx, f, KindIncident, recent)
	case paused:
		if err := l.recordEvent(ctx, tx, f, "suppressed", nil, "paused"); err != nil {
			return Admission{}, err
		}
		return Admission{Outcome: OutcomeSuppressed}, l.countOnly(ctx, tx, f)
	}
	return l.attach(ctx, tx, f, kind, 1)
}

func (l *Ledger) newAction(f Failure, kind, protocol string) NewAction {
	now := l.now()
	return NewAction{WorkspaceID: f.WorkspaceID, RepoOwner: f.RepoOwner, RepoName: f.RepoName, OriginalRunID: f.RunID,
		Kind: kind, Protocol: protocol, SignatureHash: f.SignatureHash, DueAt: now, RunCompletedAt: now}
}

func (l *Ledger) recordEvent(ctx context.Context, tx pgx.Tx, f Failure, kind string, pausedUntil *time.Time, detail string) error {
	_, err := tx.Exec(ctx, `INSERT INTO ci_watch_event (workspace_id, repo_owner, repo_name, signature_hash, kind, run_id,
		paused_until, detail, created_at) VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9)`,
		f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, kind, f.RunID, pausedUntil, detail, l.now())
	return err
}

// countOnly bumps the counter of an existing owner (paused runs schedule nothing).
func (l *Ledger) countOnly(ctx context.Context, tx pgx.Tx, f Failure) error {
	var issueID *string
	var count int
	err := tx.QueryRow(ctx, `UPDATE ci_signature_owner SET fail_count=fail_count+1, last_run_url=$5
		WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4
		RETURNING issue_id::text, fail_count`, f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, f.RunURL).Scan(&issueID, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil || issueID == nil {
		return err
	}
	return syncIssueMetadata(ctx, tx, f.WorkspaceID, *issueID, count, f.RunURL)
}

var kindRank = map[string]int{KindP3: 0, KindP2: 1, KindIncident: 2}

// attach claims the signature owner for a new issue, or counts the run on the
// existing one. kind only moves up (p3 < p2 < incident). initial is the failure
// count a freshly claimed owner starts with.
func (l *Ledger) attach(ctx context.Context, tx pgx.Tx, f Failure, kind string, initial int) (Admission, error) {
	var cur string
	err := tx.QueryRow(ctx, `SELECT kind FROM ci_signature_owner WHERE workspace_id=$1::uuid AND repo_owner=$2
		AND repo_name=$3 AND signature_hash=$4`, f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash).Scan(&cur)
	reopen := ReopenAfter
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Admission{}, err
	case cur == KindIncident:
		reopen = ReopenAfterIncident
	}
	claimed, err := l.ClaimSignatureOwner(ctx, tx, f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, reopen)
	if err != nil {
		return Admission{}, err
	}
	if claimed {
		if _, err := tx.Exec(ctx, `UPDATE ci_signature_owner SET kind=$5, fail_count=$6, last_run_url=$7, digest_class='',
			last_digest_at=NULL WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4`,
			f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, kind, initial, f.RunURL); err != nil {
			return Admission{}, err
		}
		_, _, err := l.schedule(ctx, tx, l.newAction(f, ActionIssueCreate, strings.ToLower(f.Protocol)))
		return Admission{Outcome: OutcomeScheduled}, err
	}

	var state string
	var issueID *string
	var count int
	err = tx.QueryRow(ctx, `UPDATE ci_signature_owner SET fail_count=GREATEST(fail_count+1, $6::int), last_run_url=$5,
			kind=CASE WHEN $7::int > (CASE kind WHEN 'incident' THEN 2 WHEN 'p2' THEN 1 ELSE 0 END) THEN $8::text ELSE kind END
		WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4
		RETURNING state, issue_id::text, fail_count`,
		f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, f.RunURL, initial, kindRank[kind], kind).Scan(&state, &issueID, &count)
	if err != nil {
		return Admission{}, err
	}
	switch state {
	case "creating":
		return Admission{Outcome: OutcomeAttached}, nil // the creator reads the kind and count when it runs
	case "closed":
		if err := l.recordEvent(ctx, tx, f, "reopen_blocked", nil, ""); err != nil {
			return Admission{}, err
		}
	}
	if issueID != nil {
		if err := syncIssueMetadata(ctx, tx, f.WorkspaceID, *issueID, count, f.RunURL); err != nil {
			return Admission{}, err
		}
	}
	// The executor decides whether a digest is due (once per 6 h, at once on a class change).
	_, _, err = l.schedule(ctx, tx, l.newAction(f, ActionIssueComment, strings.ToLower(f.Protocol)))
	return Admission{Outcome: OutcomeAttached}, err
}

// syncIssueMetadata writes the coalesced counter. A signature whose issue row
// is not in this database (no match) is not an error.
func syncIssueMetadata(ctx context.Context, tx pgx.Tx, ws, issueID string, count int, url string) error {
	_, err := tx.Exec(ctx, `UPDATE issue SET metadata = jsonb_set(jsonb_set(metadata, '{ci_watch_count}', to_jsonb($3::int)),
			'{last_run_url}', to_jsonb($4::text)), revision = revision + 1
		WHERE id=$2::uuid AND workspace_id=$1::uuid`, ws, issueID, count, url)
	return err
}

// NewStormRecheck cancels a rerun inside a pause and an issue_create over the
// daily cap, inside the claim transaction.
func NewStormRecheck(now func() time.Time) Recheck {
	return func(ctx context.Context, tx pgx.Tx, a Action) (Verdict, error) {
		switch a.Kind {
		case ActionRerun:
			var paused bool
			err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ci_watch_event WHERE workspace_id=$1::uuid
				AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4 AND kind='breaker' AND paused_until > $5)`,
				a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash, now()).Scan(&paused)
			if paused {
				return Verdict{CancelReason: ReasonBreaker}, err
			}
			return Verdict{}, err
		case ActionIssueCreate:
			cfg, err := loadConfig(ctx, tx, a)
			if err != nil {
				return Verdict{}, err
			}
			if err := lockRepoCaps(ctx, tx, a); err != nil {
				return Verdict{}, err
			}
			var made int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ci_watch_action WHERE workspace_id=$1::uuid AND repo_owner=$2
				AND repo_name=$3 AND action_kind='issue_create' AND state='done' AND updated_at > $4`,
				a.WorkspaceID, a.RepoOwner, a.RepoName, now().Add(-24*time.Hour)).Scan(&made); err != nil {
				return Verdict{}, err
			}
			if made < cfg.MaxNewIssuesPerDay {
				return Verdict{}, nil
			}
			_, err = tx.Exec(ctx, `INSERT INTO ci_watch_event (workspace_id, repo_owner, repo_name, signature_hash, kind,
				run_id, detail, created_at) VALUES ($1::uuid,$2,$3,$4,'capped',$5,'issue_cap',$6)`,
				a.WorkspaceID, a.RepoOwner, a.RepoName, a.SignatureHash, a.OriginalRunID, now())
			return Verdict{CancelReason: ReasonCap}, err
		}
		return Verdict{}, nil
	}
}
