package ciwatch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	ReasonNoPrincipal      = "no_principal"
	ReasonPrincipalRemoved = "principal_removed"
	ReasonNoCheckKey       = "no_check_key"
	ReasonRedMainClaimed   = "red_main_claimed_elsewhere"
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
	OutcomeRerun      = "rerun"      // P1 rerun scheduled
	OutcomeDuplicate  = "duplicate"  // this run was admitted before
)

// Failure is one classified failed run handed to Admit.
type Failure struct {
	WorkspaceID, RepoOwner, RepoName string
	RunID                            int64
	SignatureHash                    string
	Protocol                         string // "P1", "P2", "P3" or "P4"
	RunURL                           string
	CheckName                        string    // the red CI job or test name; keys the red-main owner
	RerunDueAt                       time.Time // P1 only: when the rerun is due; zero means now
}

type Admission struct{ Outcome string }

// Admit runs classification's follow-up in one transaction under the advisory
// lock on (repo, signature): breaker check, owner claim, action scheduling.
func (l *Ledger) Admit(ctx context.Context, f Failure) (Admission, error) {
	switch f.Protocol {
	case "P1", "P2", "P3", "P4":
	default: // an unknown protocol must not default to P2
		return Admission{}, fmt.Errorf("ciwatch: protocol %q is not admitted here", f.Protocol)
	}
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

// terminalIssue is true for an issue in a done or closed status category (alias i).
const terminalIssue = `(i.status IN ('done','cancelled') OR EXISTS (SELECT 1 FROM issue_status s
	WHERE s.workspace_id=i.workspace_id AND s.key=i.status AND s.category IN ('done','closed')))`

// reconcileClosure makes the owner row follow its real issue: a done or closed
// issue closes the owner at the time of its last status change (an edit after closure must not restart the window), a reopened one reopens it.
// The 24 h and 7 d reopen windows in claimOwner read that time.
func reconcileClosure(ctx context.Context, tx pgx.Tx, f Failure) error {
	if _, err := tx.Exec(ctx, `UPDATE ci_signature_owner o SET state='closed',
			closed_at=COALESCE((SELECT max(a.created_at) FROM activity_log a WHERE a.issue_id=i.id AND a.action='status_changed'), i.updated_at)
		FROM issue i WHERE i.id=o.issue_id AND i.workspace_id=o.workspace_id AND o.workspace_id=$1::uuid AND o.repo_owner=$2
			AND o.repo_name=$3 AND o.signature_hash=$4 AND o.state='open' AND `+terminalIssue,
		f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE ci_signature_owner o SET state='open', closed_at=NULL
		FROM issue i WHERE i.id=o.issue_id AND i.workspace_id=o.workspace_id AND o.workspace_id=$1::uuid AND o.repo_owner=$2
			AND o.repo_name=$3 AND o.signature_hash=$4 AND o.state='closed' AND NOT `+terminalIssue,
		f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash)
	return err
}

// checkKey is the red-main "one owner per check" key: the CI job or test name,
// lowercased, every character outside a-z0-9._- replaced by "-", cut to 60. The
// name comes from CI, so a key git would refuse as a ref is dropped.
var nonKeyChar = regexp.MustCompile(`[^a-z0-9._-]`)

func checkKey(name string) string {
	k := nonKeyChar.ReplaceAllString(strings.ToLower(name), "-")
	if len(k) > 60 {
		k = k[:60]
	}
	if k == "" || k[0] == '-' || k[0] == '.' || strings.Contains(k, "..") || strings.HasSuffix(k, ".lock") {
		return ""
	}
	return k
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
	if err := reconcileClosure(ctx, tx, f); err != nil {
		return Admission{}, err
	}
	// Every applicable failure counts toward the breaker before any protocol side effect.
	if err := l.recordEvent(ctx, tx, f, "admit", nil, ""); err != nil {
		return Admission{}, err
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
	case f.Protocol == "P1":
		// The rerun is scheduled in this transaction, after the admit event, so the
		// breaker counts it and a trip cancels it.
		na := l.newAction(f, ActionRerun, "p1")
		if !f.RerunDueAt.IsZero() {
			na.DueAt = f.RerunDueAt
		}
		_, _, err := l.schedule(ctx, tx, na)
		return Admission{Outcome: OutcomeRerun}, err
	case f.Protocol == "P4":
		// A notification is already deduplicated per (repo, signature, day); no owner.
		_, _, err := l.schedule(ctx, tx, l.newAction(f, ActionInboxNotify, "p4"))
		return Admission{Outcome: OutcomeNotify}, err
	case f.Protocol == "P3":
		return l.attach(ctx, tx, f, KindP3, 1)
	}
	return l.attach(ctx, tx, f, KindP2, 1)
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
// count a freshly claimed owner starts with. A failure with a check name
// follows the red-main "one owner per check" rule: a live owner of the same
// check (another signature's owner, or an open red-main issue) takes the run.
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
	key := checkKey(f.CheckName)
	if key != "" { // after the signature lock, always: one lock order
		if err := lockCheck(ctx, tx, Action{WorkspaceID: f.WorkspaceID, RepoOwner: f.RepoOwner, RepoName: f.RepoName}, key); err != nil {
			return Admission{}, err
		}
	}
	// A savepoint, so a duplicate issue_create (same run and kind scheduled
	// before) drops the claim instead of leaving an orphan `creating` owner.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Admission{}, err
	}
	defer func() { _ = sp.Rollback(ctx) }()
	claimed, err := l.claimOwner(ctx, sp, f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, reopen)
	if err != nil {
		return Admission{}, err
	}
	if claimed {
		adopt := ""
		if key != "" {
			other, err := liveCheckOwner(ctx, sp, f, key)
			if err != nil {
				return Admission{}, err
			}
			if other != "" { // the check already has an owner: drop this claim, count on that one
				if err := sp.Rollback(ctx); err != nil {
					return Admission{}, err
				}
				f.SignatureHash = other
				return l.countOnOwner(ctx, tx, f, kind, initial)
			}
			if adopt, err = openRedMainIssue(ctx, sp, f, key); err != nil {
				return Admission{}, err
			}
		}
		if _, err := sp.Exec(ctx, `UPDATE ci_signature_owner SET kind=$5, fail_count=$6, last_run_url=$7, digest_class='',
			last_digest_at=NULL, check_key=$8 WHERE workspace_id=$1::uuid AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4`,
			f.WorkspaceID, f.RepoOwner, f.RepoName, f.SignatureHash, kind, initial, f.RunURL, key); err != nil {
			return Admission{}, err
		}
		if adopt != "" { // reuse the open issue: no creation, the digest announces the run
			if _, err := sp.Exec(ctx, `UPDATE ci_signature_owner SET state='open', issue_id=$5::uuid WHERE workspace_id=$1::uuid
				AND repo_owner=$2 AND repo_name=$3 AND signature_hash=$4`, f.WorkspaceID, f.RepoOwner, f.RepoName,
				f.SignatureHash, adopt); err != nil {
				return Admission{}, err
			}
			if err := syncIssueMetadata(ctx, sp, f.WorkspaceID, adopt, initial, f.RunURL); err != nil {
				return Admission{}, err
			}
			_, inserted, err := l.schedule(ctx, sp, l.newAction(f, ActionIssueComment, strings.ToLower(f.Protocol)))
			if err != nil {
				return Admission{}, err
			}
			if !inserted {
				return Admission{Outcome: OutcomeDuplicate}, nil
			}
			return Admission{Outcome: OutcomeAttached}, sp.Commit(ctx)
		}
		_, inserted, err := l.schedule(ctx, sp, l.newAction(f, ActionIssueCreate, strings.ToLower(f.Protocol)))
		if err != nil {
			return Admission{}, err
		}
		if !inserted {
			return Admission{Outcome: OutcomeDuplicate}, nil // deferred Rollback drops the claim
		}
		return Admission{Outcome: OutcomeScheduled}, sp.Commit(ctx)
	}
	if err := sp.Rollback(ctx); err != nil {
		return Admission{}, err
	}
	return l.countOnOwner(ctx, tx, f, kind, initial)
}

// liveCheckOwner returns the signature of another owner of the check whose
// issue is still being created or is not done or closed.
func liveCheckOwner(ctx context.Context, tx pgx.Tx, f Failure, key string) (string, error) {
	var sig string
	err := tx.QueryRow(ctx, `SELECT o.signature_hash FROM ci_signature_owner o
		LEFT JOIN issue i ON i.id=o.issue_id AND i.workspace_id=o.workspace_id
		WHERE o.workspace_id=$1::uuid AND o.repo_owner=$2 AND o.repo_name=$3 AND o.check_key=$4 AND o.signature_hash<>$5
			AND o.state IN ('creating','open') AND (i.id IS NULL OR NOT `+terminalIssue+`)
		ORDER BY o.created_at LIMIT 1`, f.WorkspaceID, f.RepoOwner, f.RepoName, key, f.SignatureHash).Scan(&sig)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return sig, err
}

// redMainPrefix is the title prefix of a check's owner issue.
func redMainPrefix(repo, key string) string { return "red-main " + repo + " " + key + ":" }

// openRedMainIssue finds a not-done issue titled for the check that no owner row holds yet.
func openRedMainIssue(ctx context.Context, tx pgx.Tx, f Failure, key string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT i.id::text FROM issue i WHERE i.workspace_id=$1::uuid AND left(i.title, length($2))=$2
			AND NOT `+terminalIssue+` AND NOT EXISTS (SELECT 1 FROM ci_signature_owner o WHERE o.issue_id=i.id)
		ORDER BY i.created_at LIMIT 1`, f.WorkspaceID, redMainPrefix(f.RepoName, key)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// countOnOwner counts the run on the signature's existing owner and schedules its digest.
func (l *Ledger) countOnOwner(ctx context.Context, tx pgx.Tx, f Failure, kind string, initial int) (Admission, error) {
	var state string
	var issueID *string
	var count int
	err := tx.QueryRow(ctx, `UPDATE ci_signature_owner SET fail_count=GREATEST(fail_count+1, $6::int), last_run_url=$5,
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
